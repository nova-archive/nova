# P2-M3 — Assignment synchronization

Status: **design**. Spec floor: the P2-M0-amended normative specs in `docs/specs/`
(`FEDERATION_PROTOCOL.md` §§ `/fed/v1/pins/{changes,snapshot,{cid}/ack,{cid}/fail}`,
`DATA_MODEL.sql` D6/D7 commentary), the Phase-2 master design
([`2026-06-11-phase2-federation-design.md`](2026-06-11-phase2-federation-design.md)
§§ "Binding constraints for M3", "Schema deltas"), and the P2-M2
identity/registration design
([`2026-06-16-phase2-m2-identity-registration-design.md`](2026-06-16-phase2-m2-identity-registration-design.md)).
Implementation plan:
[`../../plans/phase2/2026-06-22-phase2-m3-assignment-sync.md`](../../plans/phase2/2026-06-22-phase2-m3-assignment-sync.md).

Authors: Bug Plowman (operator), Claude (implementation partner).

## Context

Phase 2 makes Nova's data durable across volunteer-run **donor nodes** over a
private Nebula mesh. **P2-M2 is merged** (tag `p2-m2-identity-registration`): the
live mTLS control channel — `POST /fed/v1/register` + `/fed/v1/heartbeat`, node
identity from the verified federation certificate, fail-closed capability
negotiation, `trust_state` assignment, and the `novactl node` CA/cert/revocation
tooling. The heartbeat handler already returns `current_epoch: 0` with the
comment *"M3 gives this meaning."* **There are no pins yet:** nothing creates an
assignment, no change log exists, and the donor's local state store is an
in-memory stub.

**P2-M3 lights up assignment synchronization — the first durable distributed-state
protocol in Nova.** The coordinator records which CIDs each donor *should* hold
as an append-only change log; each donor polls that log (or recovers via a
paginated snapshot), and durably converges a **local desired-assignment set**
with crash- and long-offline recovery. The master design's milestone line:

> **P2-M3** — Assignment synchronization. `pin_changes` log + retention;
> snapshot/epoch recovery + `snapshot_required`; node-local cursor;
> `assignment_id`/`generation`; idempotent apply; ack/fail/unpin state machine;
> crash + long-offline recovery tests.

**No bytes move in M3.** Coordinator-as-source transfer, Ed25519 repair-token
minting, deterministic re-import, donor pinning, and the production donor `ack`
are **P2-M4**. M3 builds the *control plane* — the trustworthy, recoverable
assignment ledger that M4 hangs the data path onto. This boundary is the
hourglass "waist" between Nova's control plane (registration / heartbeat /
assignment sync) and its data plane (transfer / repair / streaming AEAD): M3
keeps that waist narrow and refuses to widen it with stub transfer behavior
before the data-plane contract exists.

## Decisions (ratified with Bug, 2026-06-22)

### D-M3-1 — Donor scope is sync-only; no fetch, and no `ack`.

The M3 donor registers/heartbeats as in M2, polls `/fed/v1/pins/changes`, applies
changes idempotently, durably persists its **desired-assignment set** + cursor,
and recovers via `/fed/v1/pins/snapshot` when its cursor falls behind retention.
It does **not** fetch ciphertext and **does not call `ack`**.

The reason is correctness, not convenience. In the protocol an `ack` asserts the
donor *successfully pinned the CID locally and verified it by deterministic
re-import + root-CID comparison* (D4). A donor that has only **observed an
assignment** has acquired no evidence that it **holds** the bytes — collapsing
"saw assignment" into "verified holder" mislabels a weak signal as a strong one.
A false `ack` would poison the acked-holder set that every later subsystem trusts
as durability ground truth: M4 read-source selection and commit quorum, M5
healing and origin pruning, M6 possession audits. The `ack`/`fail` endpoints and
the coordinator's conditional state machine **are** built and tested in M3 — but
exclusively through the test client / fixtures, never through donor auto-ack. The
production `assign → fetch → verify → pin → ack` loop is wired in M4.

### D-M3-2 — Three distinct state concepts, named so they cannot be confused.

The milestone deliberately separates intent from evidence:

| Concept | Where it lives | Meaning |
|---|---|---|
| **desired assignment** | donor local set; `pin_assignments` rows; `pin_changes` log | the coordinator's *intent* that this node hold this CID — durably learned, not yet verified |
| **acked assignment** | `pin_assignments.state = 'acked'` | the donor *claims verified local storage* of the CID (only ever set via the `ack` endpoint; in M3 only via test fixtures) |
| **eligible read source** | derived query (M4/M5) | `acked` **and** node not revoked **and** sufficiently live/trusted — the set M4 fetches from |

Operator-facing output uses this vocabulary verbatim. `novactl pin list` prints
**"desired assignments"** and **"verified holders"** as separate lines; in M3 the
verified-holders line is empty under normal operation (it only populates once M4
donors really ack). This keeps an operator, a test, or the future M5 scheduler
from reading assignment intent as storage reality.

### D-M3-3 — Assignment creation is a reusable coordinator transaction, surfaced by `novactl pin`.

The M5 placement/scheduler does not exist, so M3 needs a real, testable way to
create `assign`/`unpin` changes. The core logic lives in an **exported
coordinator transaction seam** (`AssignPin` / `UnpinPin`); `novactl pin
assign|unpin|list` is a thin DB-direct wrapper over it, matching the established
`novactl collection create` / `novactl node` pattern (the `withNodeDB` helper in
`cmd/novactl/node_db.go`). The CLI is a low-level **manual / test / operator
override** — no replication factor, no node selection (that is M5). M5's
scheduler will call the *same* seam rather than re-implementing assignment
mutation, so the atomicity + generation rules have exactly one home.

### D-M3-4 — Migration `0012` is sync-only and read-selection-ready; M5/M6 schema stays deferred.

`0012_assignment_sync.sql` adds only what M3 *owns*: `pin_assignments`
versioning, the `pin_changes` log, the retention watermark, and `byte_size` on
changes. It **defers** `blob_replication_state`, the `nodes` D8/D9 placement
columns (`failure_domain_id`, `placement_weight`, `provider`, `asn`,
`donor_principal_id`, `operator_verified_at`), and the `pin_audits`
receive-time/sampling columns to M5/M6 — schema lands when the milestone that
*maintains* it lands (the principle M2 adopted when it deferred D8/D9). A
projection nobody updates is worse than no projection: it invites code to trust
stale data. The one forward obligation M3 honors is the master design's binding
constraint that pin/holder state be **query-shaped for read selection**: "which
live `acked` donors hold CID X, ranked by reputation" must be cheap. It already
is — `pin_assignments_cid_state_idx (cid, state)` joined to
`nodes.reputation_score` answers it — so M3 preserves that index and ships the
*query itself in M4*, where it is consumed.

## Scope

**In scope.** Migration `0012`; `pin_changes` + assignment versioning; the
coordinator endpoints `GET /pins/changes`, `GET /pins/snapshot`, `POST
/pins/{cid}/ack`, `POST /pins/{cid}/fail`; the `AssignPin`/`UnpinPin` transaction
seam + `novactl pin assign|unpin|list`; change-log retention/prune +
`snapshot_required`; the epoch / snapshot-consistency model; the donor durable
cursor + desired-set + snapshot recovery + idempotent apply + fail-closed unknown
kinds; `wire` reconciliation; the structured-log observability signal set; crash
+ long-offline recovery tests.

**Out of scope (owning milestone).** Donor blob fetch / Kubo pinning / production
`ack`, coordinator-as-source transfer, donor↔donor repair, repair-token minting
(**M4**); liveness sweeper, healing scheduler, placement / failure-domain policy,
`blob_replication_state` projection (**M5**); possession audits, reputation
graduation (**M6**); a Prometheus `/metrics` endpoint (**M7** — the slog signal
set below is defined so M7's promotion is mechanical).

## Source of truth / wire reconciliation

The M1 `wire` stubs are thinner than the normative JSON in `FEDERATION_PROTOCOL.md`.
M3 reconciles `internal/federation/wire/messages.go` to the spec while keeping the
package dependency-free (donor-safe; no operator-only imports):

- `PinChange`: tag the sequence field `json:"seq"`; add `ByteSize int64
  json:"byte_size"`; add `Source *ChangeSource json:"source,omitempty"` —
  **nil in M3**, documented as M4-populated (the repair `{node_id, nebula_addr,
  token}`), so M4 needs no second wire bump.
- `ChangesResponse`: rename `CurrentSeq` → `NextSeq json:"next_seq"`; keep
  `CurrentEpoch json:"current_epoch"`.
- New `SnapshotItem{CID, AssignmentID, Generation, ByteSize, AssignedAt}` and
  `SnapshotResponse{Data, Cursor, SnapshotEpoch}`.
- `Ack`: add `ByteSize`, `IPFSPinStatus`, `FetchedFromNodeID` (omitempty).
  `Fail`: add `Details`; define `reason` constants (`out_of_space`,
  `blob_unavailable`, `policy_filter`, `network_error`, `kubo_error`,
  `source_unauthorized`, `cid_mismatch`, `budget_exceeded`, `other`).
- Error codes: `CodeSnapshotRequired` and `CodeUnknownChangeKind` already exist;
  add `CodeStaleAssignment` for generation-superseded `ack`/`fail`.

## Architectural notes

### Change-log ordering, cursor, and epoch

The specs fix the wire fields (`since_seq`, `next_seq`, `current_epoch`,
`snapshot_epoch`, `snapshot_required`) but leave the sequencing model to the
milestone. The model below is correct against those fields and avoids the two
classic change-log hazards (sequence gaps under concurrency; empty-node recovery
loops).

- **Global `bigserial` sequence.** `pin_changes.sequence` is monotonic across all
  nodes; a donor's cursor lives in this global space and sees only its own node's
  (sparse) rows.
- **Commit-order = sequence-order (commit-order-safe).** The assignment seam
  takes a fixed `pg_advisory_xact_lock` before inserting into `pin_changes`, so
  `bigserial` values commit in assignment order. Without it, a txn that drew a
  lower sequence could commit *after* one with a higher sequence, and a donor
  polling in between would set its cursor past the lower row and never see it.
  (Gaps from rolled-back txns are harmless — the cursor uses `sequence >
  since_seq` — so the guarantee is commit ordering, not gap-freedom.) M3/M5
  write volume is low, so the lock is cheap insurance against silent assignment
  loss.
- **`/changes` response.** `next_seq` = the last returned `seq` when the page is
  full (more may exist) else the global change-log head (caller is caught up);
  `current_epoch` = global change-log head (`COALESCE(MAX(sequence),0)`).
- **`snapshot_required`.** Returned (HTTP 400, `code:"snapshot_required"`) iff
  `since_seq < pruned_through_seq`. After recovery the donor adopts the snapshot's
  `snapshot_epoch` (= global head, always ≥ the watermark) as its cursor, so an
  empty or brand-new node does **not** loop back into `snapshot_required`.
- **Snapshot pagination consistency.** Page 1 captures `snapshot_epoch` = global
  head. Later pages echo it; the coordinator returns **409 Conflict** (with the
  new epoch) iff *this node* has any `pin_changes` row with `sequence >
  snapshot_epoch` — only this node's own mutations force a restart, not unrelated
  nodes' activity. The donor restarts from page 1.

### Coordinator endpoints + state machine

Handlers live in `internal/federation/coordinator/handlers.go`; routes register in
`server.go`'s `mux()` using Go 1.22+ method-qualified `{cid}` patterns
(`r.PathValue("cid")`). All reuse the existing `authenticate()` (verified-cert
identity) and `writeJSON` / `writeError` helpers, and reject revoked nodes
exactly as register/heartbeat do (trust-state boundary).

- `GET /fed/v1/pins/changes` — watermark check → `GetPinChangesSince(node,
  since_seq, limit)` → compute `next_seq` + `current_epoch`.
- `GET /fed/v1/pins/snapshot` — capture/verify `snapshot_epoch` (409 on this-node
  change beyond it) → `GetPinSnapshotPage(node, after_cid, limit)` (joins `blobs`
  for `byte_size`) → `cursor` = last `cid`.
- `POST /fed/v1/pins/{cid}/ack` — conditional `UPDATE … SET state='acked',
  acked_at=now() WHERE cid=$ AND node_id=$ AND assignment_id=$ AND generation=$
  AND state='pending'`. **1 row → 204**; **0 rows →** 204 if the row is already
  `acked` at the *same* generation (idempotent replay), else **409
  `stale_assignment`**; unknown assignment → **404**. A delayed `ack` for a
  superseded generation can never resurrect a stale assignment (D6).
- `POST /fed/v1/pins/{cid}/fail` — conditional transition to `failed`; `204`.
- `handleHeartbeat` now returns the real `current_epoch` (global head) and a
  default `pins_poll_interval_seconds`.

### Assignment seam + `novactl pin`

`internal/federation/coordinator/assignments.go` exports the transactional core:

- `AssignPin(ctx, tx, cid, nodeID)` — advisory lock; upsert `pin_assignments`
  (`ON CONFLICT (cid,node_id)` → bump `generation`, reset to `pending`, **keep the
  existing immutable `assignment_id`**; on insert mint a fresh `assignment_id`,
  `generation = 1`); append a `pin_changes` row (`kind='assign'`, matching
  generation, `byte_size` from `blobs`).
- `UnpinPin(ctx, tx, cid, nodeID)` — advisory lock; append `pin_changes`
  (`kind='unpin'`, `generation`+1); delete the `pin_assignments` row. The history
  survives in the log; the live table holds only current assignments, so the
  snapshot query is a plain scan of `pin_assignments`.

`cmd/novactl/pin.go` wraps the seam DB-direct via `withNodeDB`:

```
novactl pin assign --cid <cid> --node <node-id>
novactl pin unpin  --cid <cid> --node <node-id>
novactl pin list   --cid <cid> | --node <node-id>
```

`pin list` prints **desired assignments** and **verified holders** as separate,
explicitly labelled lines (D-M3-2).

### Donor: durable local state + sync loop

Donors hold no Postgres. New atomic-JSON file stores under `storage_dir/state/`
(mirroring `FileRegistrationStore`):

- `FileStore` implements the existing `state.Store` (durable `Cursor`; the `jti`
  replay cache may stay in-memory — single-use tokens are M4).
- `FileAssignmentStore` holds the desired set `cid → {assignment_id, generation,
  byte_size, state}` with `ApplyChange`, `ReplaceFromSnapshot`, `List`. **Write
  order: set first, then cursor** — the cursor trails persisted state so a crash
  can re-deliver but never skip.

`internal/node/agent/agent.go` gains a second `pinsPoll` ticker
(`pins_poll_interval_seconds`) and extends the `Client` interface with
`GetChanges` + `GetSnapshot` (not `Ack`/`Fail` — D-M3-1). Each tick:
`GetChanges(cursor)`; on `snapshot_required` run the snapshot-recovery loop
(paginate, restart on 409), `ReplaceFromSnapshot`, set cursor = `snapshot_epoch`;
else apply changes in `seq` order **idempotently** (keyed by `(assignment_id,
generation)`: apply only if incoming `generation` ≥ stored), an **unknown `kind`
fails closed** (stop, log, force a snapshot resync), then persist set and advance
cursor to `next_seq`. The donor advertises `pin-change-log/v1` + `snapshot/v1` at
register; the coordinator requires them by default in M3 (operator-tunable).

### Retention / prune

A coordinator background goroutine (started in `Server.Run`, tickered) deletes
`pin_changes` older than `federation.change_log_retention` (config; default
`168h`) and advances `pruned_through_seq` to the max pruned `sequence`. This makes
the long-offline recovery path real and testable (prune, then assert a stale
cursor receives `snapshot_required`).

### Observability signal set (structured slog)

Nova's observability convention is `log/slog` (no first-class metrics framework
yet). M3 emits a defined signal set at the key seams so the first transfer
milestone is not debugging blind; each signal is named for the USE/RED concept it
will become when **P2-M7** promotes it to Prometheus:

| Seam | slog event (fields) | USE/RED |
|---|---|---|
| `/pins/changes` | `fed.changes.served` (`node_id`, `since_seq`, `returned`, `next_seq`, `dur_ms`); `fed.changes.snapshot_required` (`node_id`, `since_seq`) | RED rate/errors/duration |
| `/pins/snapshot` | `fed.snapshot.page` (`node_id`, `epoch`, `returned`); `fed.snapshot.conflict` (`node_id`, `epoch`) | RED + errors |
| ack/fail | `fed.ack.applied` / `fed.ack.stale` (`assignment_id`, `generation`); `fed.fail.applied` (`reason`) | RED + error class |
| change log | `fed.changelog.append` (`seq`, `kind`); `fed.changelog.pruned` (`count`, `pruned_through_seq`) | USE saturation |
| assignment seam | `fed.assign.txn` (`cid`, `node_id`, `generation`, `dur_ms`, `rolled_back`) | RED + errors |
| donor | `node.sync.applied` (`from_seq`, `to_seq`, `applied`); `node.sync.recovered` (`snapshot_epoch`, `items`); `node.sync.failclosed` (`kind`); `node.state.write_error` | USE errors + cursor lag |

`novactl pin list`/`status` exposes change-log head, `pruned_through_seq`, and
per-node cursor lag for ad-hoc operator inspection without a metrics backend.

## Component ownership

| Concern | Coordinator (operator) | Donor | Notes |
|---|---|---|---|
| `pin_changes` log + retention + watermark | ✓ | — | durable source of truth |
| `pin_assignments` versioning + state machine | ✓ | — | conditional, generation-keyed |
| changes / snapshot / ack / fail endpoints | ✓ (serve) | ✓ (consume changes/snapshot only) | donor never acks in M3 |
| assignment creation (`AssignPin`/`UnpinPin`) | ✓ | — | reused by M5 scheduler |
| desired-assignment set + cursor (durable, no Postgres) | — | ✓ | atomic-JSON file store |
| snapshot recovery + idempotent apply + fail-closed | — | ✓ | crash/offline resilient |
| blob fetch / verify / production ack | — | — | **M4** |

## Forward-compatibility

- **M4** populates `PinChange.Source` + repair tokens, adds the donor `assign →
  fetch → verify → pin → ack` loop on top of M3's desired set, and ships the
  "eligible read source" query over M3's preserved `(cid, state)` index.
- **M5** adds `blob_replication_state` (maintained in the same txn as the
  assignment seam), the D8/D9 placement columns, and the scheduler that calls
  `AssignPin`/`UnpinPin`.
- **M7** promotes the slog signal set to a Prometheus `/metrics` surface.
- **Phase 6 HA** treats the donor cursor as the resumption point a donor
  preserves when failing over between coordinators — the global-sequence model is
  already compatible.

## Exit criterion

A registered donor durably converges its **desired-assignment set** from
change-log polling or snapshot recovery; the coordinator creates `assign`/`unpin`
changes atomically and rejects stale or unknown `ack`/`fail` by generation
(fail-closed); the coordinator can distinguish **desired assignments** from
**verified holders**; crash and long-offline recovery are covered by tests. **No
bytes move and no production donor `ack` occurs** — that is M4.
