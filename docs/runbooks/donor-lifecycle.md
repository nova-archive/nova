# Runbook: donor lifecycle (revoke / suspend / drain / below-floor)

> **P2-M7 (D-M7-4/D-M7-6).** Operator-executable procedures for donor
> departures and trust interventions. Every signal named here is on the
> coordinator's `/metrics` plane (default `127.0.0.1:2112`, D-M7-1) or in
> `novactl`. The automated proofs behind this runbook are
> `internal/orchestrator/drills_test.go` and
> `internal/federation/e2e/m7_drain_test.go`.

## Choosing the instrument

| Situation | Instrument | Effect |
|---|---|---|
| Hostile, compromised, or lying donor (failed hash audit, stolen cert) | `novactl node revoke` **now** | Cert refused at next request; replicas drop from durability instantly; CIDs reconcile+heal from other sources. Involuntary, immediate. |
| Questionable donor you want paused but not destroyed (operator judgment call, pending review) | `novactl node trust` → **suspend** | Stops read-source eligibility and placement; replicas still count toward durability. Reversible without data movement. |
| Donor leaving on good terms | `novactl node drain` | Voluntary two-step decommission (below). Replicas re-replicate FROM the leaver before it goes. |

Drain is never the tool for a hostile node — a hostile node must not remain a
read/repair source, which is exactly what a draining node stays.

## Graceful decommission (drain → watch → revoke)

1. **Drain:**

   ```sh
   novactl node drain --id <node-id>
   ```

   Refusals: a non-`active`/`suspect` node cannot drain (use revoke); a node
   whose `assignment_sync_state != current` needs `--force` (its CID set is
   still stabilizing — prefer waiting). The command prints the opening drain
   debt. Draining is idempotent: re-running re-enqueues the node's CIDs and
   preserves the original `draining_at`.

2. **Tell the volunteer to keep the donor RUNNING.** A draining node is a
   deprioritized read source and a repair source of last resort — for a CID it
   holds alone, it is the ONLY place the bytes can come from.

3. **Watch the debt drain:**

   - `nova_node_drain_pending_cids{node_id=...}` — CIDs still below target
     without this node. Must reach 0.
   - `nova_node_drain_inflight_cids{node_id=...}` — how many of those have a
     pending replacement on an eligible destination ("working" vs "stuck").
   - `nova_node_drain_pending_oldest_seconds{node_id=...}` — how long the
     drain has been open while debt remains.
   - `nova_node_drain_ready{node_id=...}` — 1 when debt is 0.

   Stuck drain (debt > 0, inflight 0, oldest climbing) usually means no
   eligible destination: check `nova_nodes` for spare active/current capacity
   and `nova_reconcile_queue_depth`/`nova_reconcile_queue_oldest_seconds` for
   a backed-up queue.

4. **The safe-to-revoke gate (all three, D-M7-6f):**
   1. `nova_node_drain_pending_cids == 0`;
   2. no Tier-1/Tier-2/donor_lost CID (`nova_replication_cids{tier=...}`)
      whose only usable source is the draining node;
   3. the reconcile queue is bounded and moving
      (`nova_reconcile_queue_oldest_seconds` not monotonically climbing).

5. **Revoke and tear down:**

   ```sh
   novactl node revoke --id <node-id>
   ```

   Then the volunteer may `docker compose down -v` and delete the data dir.
   Revocation after a clean drain loses zero durability — the drain e2e
   capstone proves the healthy count is unchanged end-to-end.

## Mistaken drain

```sh
novactl node undrain --id <node-id>
```

Clears the marker and re-enqueues the node's CIDs; countability and placement
eligibility return on the next recompute. Undrain is only for a node that is
NOT actually leaving — never "undrain to make the metrics quiet".

## Below-floor replica debt

**As of P2-M7.1 this is automated** (D-M7.1-3). The bulk replacement that was
a manual runbook through P2-M7 is now a healing-tick sweep; the metrics below
are for watching it work and deciding when to intervene anyway.

### The metrics

- `nova_below_floor_replica_debt` (total) /
  `nova_node_below_floor_replicas{node_id=...}` — acked replicas held on nodes
  below the floor. With the sweep enabled this should **trend to zero** as
  sustained nodes' replicas are replaced and demoted.
- `nova_below_floor_nodes{state="sustained"|"in_grace"}` — nodes carrying a
  below-floor marker. `in_grace` nodes still count toward durability (their
  reputation dipped only recently); `sustained` nodes (marker older than
  `grace`) are excluded from counts + placement and are being replaced.
- `nova_below_floor_requeued_total` — CIDs the sweep has enqueued for
  re-replication (process-local; resets on restart).
- Queue pressure shows up as `nova_reconcile_queue_depth{reason="below_floor"}`.

### How the automated sweep behaves

Each healing tick the coordinator: (1) stamps `below_floor_since` on nodes that
have just sunk below `orchestrator.reputation_floor` and clears it once a marked
node recovers past `floor + hysteresis_margin` (default 0.05 — wobble near the
floor never flaps); (2) once a marker is **sustained** past `grace` (default
24h), bounded-requeues that node's CIDs (`requeue_batch`, default 500, walked
**after** `donor_lost`/`tier1` so emergencies win); (3) fails the untrusted
assignment only once a trusted replacement has restored the healthy count —
**replace-then-demote, never a durability dip**, and a SOLE holder is never
demoted (it stays the repair source of last resort).

### Knobs (`below_floor_replacement` in operator.yaml; `enabled` + `grace` also on `/settings`)

- `enabled` (default `true`) — set `false` to keep marker maintenance + the
  gauges but pause the requeue/demote remedy (e.g. during a mass-reputation
  event you want to hand-manage).
- `grace` (default `24h`) — how long a node must stay below the floor before its
  replicas are replaced. Raise it if reputation is noisy in your fleet.
- `hysteresis_margin` (default `0.05`) — the recovery gap.
- `requeue_batch` (default `500`) — per-tick requeue cap across all sustained
  nodes; lower it to throttle re-replication bandwidth.

### When to intervene anyway

- **Leave the sweep to it** when the node is merely slow/flaky and audits still
  pass — reputation recovers with passing audits and the marker clears before
  grace even elapses.
- **Drain** when you no longer want the node long-term but it is honest — drain
  is the graceful, operator-initiated path and replaces its replicas from
  itself; it outranks below-floor in selection.
- **Revoke** when audits show `mismatch`
  (`nova_audit_results_total{result="fail",reason="mismatch"}`) or the trust
  review flags it — a lying node's replicas are already suspect; don't wait for
  grace.
- **Suspect a stuck sweep** if `nova_below_floor_replica_debt` is flat and
  non-zero while `nova_reconcile_queue_depth{reason="below_floor"}` climbs:
  usually there is nowhere trusted to place the replacements (check free
  capacity and that other nodes are above the floor), or the remedy is disabled.
