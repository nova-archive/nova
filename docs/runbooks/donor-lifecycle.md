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

`nova_below_floor_replica_debt` (total) and
`nova_node_below_floor_replicas{node_id=...}` count acked, countable replicas
held on live nodes whose reputation has sunk below the floor.

**Why these replicas still count:** below-floor excludes a node from NEW
placement and deprioritizes it as a read source, but present acked replicas
stay durability-countable until a pin-specific hard failure invalidates them.
The automated bulk replacement of below-floor replicas is **P2-M6.1** —
deliberately NOT this release (D-M7-5); M7 gives you the number and this
runbook.

When the debt is non-zero:

- **Leave alone** when the node is merely slow/flaky and audits still pass —
  reputation recovers with passing audits.
- **Drain** when you no longer want the node long-term but it is honest — the
  graceful path above replaces its replicas from itself.
- **Revoke** when audits show `mismatch` (`nova_audit_results_total{result="fail",reason="mismatch"}`)
  or the trust review flags it — a lying node's replicas are already suspect.
