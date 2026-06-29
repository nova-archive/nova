# P2-M5 Liveness & Healing — Implementation Plan

> **For agentic workers:** execute with `superpowers:subagent-driven-development`
> — one task at a time, strict TDD (write failing test → run it, see red →
> implement → green → commit). Milestone workflow: feature branch
> `p2-m5-liveness-healing`, local fast-forward merge + annotated tag
> `p2-m5-liveness-healing`, **no remote push**.

Design: [`../../specs/phase2/2026-06-28-phase2-m5-liveness-healing-design.md`](../../specs/phase2/2026-06-28-phase2-m5-liveness-healing-design.md)
(decisions D-M5-1…14, plus -RE, -8e). Read it first; every task cites the decision
it implements.

## Goal

Keep the verified donor replica set **alive**: detect donor failure via a 5-state
liveness sweeper, restore Tier-1 durability through budget-respecting **donor↔donor**
repair, place replicas to resist correlated loss, and surface degraded /
concentration signals — without overriding a donor's bandwidth budget or assuming
the coordinator still holds a copy (M4.1 prunes it).

## Architecture / tech stack

Go (go.mod 1.26.2). Coordinator: chi, pgx/sqlc, `pkg/coordinator/*`, new
`internal/orchestrator` (liveness sweeper + healing tick loop + concentration) and
`internal/notify` (webhook `Notifier`). Placement consolidates into
`pkg/coordinator/admission`. Donor (`cmd/node`): dependency-light agent +
generalized inbound mTLS source server (`internal/node/source`) now serving
donor↔donor repair, Kubo sidecar (`internal/node/ipfsclient`). Federation:
mTLS-over-Nebula, Ed25519 grants (`internal/federation/{wire,tokens,replay,transport}`),
donor-safe `wire`. Durable state: Postgres goose migration `0014`. Config: static
`internal/config` + hot-reload `reload.Store` + admin `/settings`.

## Global constraints

- **Donor boundary.** `cmd/node` + the generalized source server import only
  donor-safe packages (`wire`, `transport`, `replay`, `ipfsclient`, `bandwidth`,
  `node/*`) — never `internal/ipfs`, `internal/orchestrator`, `internal/notify`, or
  operator-only code. `bash scripts/check_node_deps.sh` (`donor-deps-boundary`)
  stays green; only reviewed entries (repair-caller acceptance + egress telemetry)
  are added.
- **One migration.** `migrations-frozen` lifts only for `0014`. Run `make
  sqlc-generate` after SQL changes; `go test ./internal/db/migrations/...` for the
  up/down + frozen checks; append `0014` to `internal/db/migrations/MANIFEST.sha256`.
- **Acked-only, countable-only durability (D5/T1.14).** Durability counts acks on
  **countable** nodes only — `status IN ('active','suspect')` AND
  `assignment_sync_state='current'`. Pending never lifts Tier-1. Cache/origin copies
  never count toward `R`.
- **Inviolable budgets (D11/T1.12).** The donor-local `bandwidth.Bucket` is
  authoritative; the coordinator only reserves best-effort and consumes telemetry as
  a hint. No doomsday override anywhere.
- **Donor-blind (T1.26).** Repair moves ciphertext under coordinator-minted grants;
  the destination donor verifies by deterministic re-import + root-CID **before**
  pin/ack; the coordinator stays the sole decrypt/serve point.
- **Single-leader-per-process.** No leader election; orchestrator runs once per
  coordinator process. Repair tokens carry per-assignment `generation`.
- gofmt only touched files; golangci-lint CI-only. slog now; Prometheus is M7.
- **Per-task done bar:** `go build ./...`, the task's `go test` target, and (for
  donor-touching tasks) `bash scripts/check_node_deps.sh` all green before commit.
- **Commit trailer:** end every commit body with
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

## Preconditions (verified during exploration; file:line are real)

- **Liveness/registration (coordinator):** `node_status` enum is already 5-state
  (`internal/db/migrations/0001_init.sql:94`). `RevokeNode` is **UPDATE-only**
  (`internal/db/gen/federation.sql.go:687` — `SET status='revoked', cert_revoked_at,
  last_status_change_at`; **no** `pin_assignments` delete). `UpdateNodeHeartbeat`
  (`:722`) sets only `last_seen_at`/free/stored bytes/`source_nebula_addr` —
  **never** `status`. `RegisterNode` (`:600`) `INSERT … status='active'` but ON
  CONFLICT does **not** update `status`. Only the heartbeat handler calls
  `UpdateNodeHeartbeat` (`internal/federation/coordinator/handlers.go:163`).
- **Assignment seam:** `AssignPin(ctx, tx, cid, nodeID)`
  (`internal/federation/coordinator/assignments.go:27`) takes a **global**
  `AcquireChangeLogLock(ctx)` at `:29` to serialize change-log sequence commit
  order — this is **not** a per-CID lock. `InsertPinChange` at `:42,65` has **no
  source field**. `mintSource` (`internal/federation/coordinator/pins.go:319`) mints
  the per-serve token from `pin_changes.byte_size`/`ChangeSource`.
- **Snapshot:** `SnapshotItem{CID,AssignmentID,Generation,ByteSize,AssignedAt}`
  (`internal/federation/wire/messages.go:152`) — **no source**; `getPinSnapshotPage`
  (`internal/db/gen/federation.sql.go:293`) projects no source. `pin_assignments` PK
  `(cid,node_id)` + `assignment_id`/`generation` (0012); `pin_state` enum has **no**
  `retired` (`0001_init.sql:102`).
- **Wire (donor-safe):** `Claims{JTI,AssignmentID,Generation,CID,SourceNodeID,
  DestNodeID,NotBefore,NotAfter,MaxBytes,ProtocolVersion}` marshals deterministically
  (`internal/federation/wire/token.go:26-37`); `HeartbeatRequest{FreeBytes,
  StoredBytes,SourceNebulaAddr}` (`messages.go:104`); `wire.CapBlobTransfer`, reserved
  `wire.CapRepairStream = "repair-stream/v1"`, M4.1 `wire.CapReadSource =
  "read-source/v1"`; `wire.Verify`; `wire.CoordinatorSourceID`. Token mint
  `internal/federation/tokens`; replay `internal/federation/replay`.
- **Donor source/transfer:** the M4.1 source server verifies the token assignment
  against the **source's own** local progress —
  `prog.State == state.ProgressAckDelivered && prog.AssignmentID == claims.AssignmentID
  && prog.Generation == claims.Generation` (`internal/node/source/server.go:173-176`);
  `io.Copy(w, io.LimitReader(rc, size+1))` then post-hoc oversize detect (`:230`).
  Role parser `transport.IdentityFromCert` → `{Role(RoleNode|RoleCoordinator),
  NodeID, FP}` (`internal/federation/transport/identity.go:17-60`).
  `internal/node/transfer` fetch→reimport→verify→pin→persist→ack;
  `internal/node/bandwidth/bucket.go:40` `Take(n,now) bool` (only method).
- **Placement / projection / classes:** `pkg/coordinator/admission/assigner.go` is
  the donor placement bridge (comment: "Anti-affinity, healing … are M5/Task 11");
  `internal/api/handlers/admission.go` is **upload-concurrency** admission (out of
  M5). M4.1 sourceable filter has a **time** predicate
  `n.last_seen_at > now() - make_interval(secs => $2::float)`
  (`internal/db/gen/storage_state.sql.go:43,192`). `blob_storage_state`
  (`internal/db/migrations/0013_storage_read_redirect.sql:8`) is **authoritative for
  `durability_class`** — `CHECK (durability_class IN ('important','normal','cache'))`,
  default `'normal'` (`:11`), plus `commit_state`/`local_role`/`local_present`/
  `cache_segment`. **No `blob_replication_state` and no `internal/orchestrator`
  exist.**
- **Config:** `Orchestrator{Replication.Factor(5/3/2),MassCasualty*,
  CapacityRunwayFloorDays}` (`internal/config/types.go:80`),
  `Federation{...timers,RepairTokenTTLSeconds}` (`:99`), `PruneStaleSeconds` (`:258`,
  default 3600 `:352`), `WebhookDestination{URL,Events,Secret(secret_file)}` (`:383`),
  `DefaultReplicationImportant=5` (`:413`). `applyReplicationDefaults`
  (`internal/config/operator_yaml.go:47`) + **important-only** range check (`:144`).
  Drift: `docs/specs/ARCHITECTURE_DECISIONS.md:161` (`important` default `3`→`5`).

## File structure

**Create:** `internal/db/migrations/0014_liveness_healing.sql`;
`internal/db/queries/replication.sql`, `internal/db/queries/notify.sql`;
`internal/orchestrator/{orchestrator.go,liveness.go,scheduler.go,projection.go,concentration.go,runway.go}`;
`internal/notify/{notifier.go,emitter.go,suppress.go}`;
`pkg/coordinator/admission/placement.go`.

**Modify:** `internal/db/queries/federation.sql`,
`internal/federation/coordinator/{assignments.go,pins.go,handlers.go}`,
`internal/federation/wire/{messages.go,token.go}`, `internal/node/source/server.go`,
`internal/node/{agent/agent.go,agent/client.go,transfer/transfer.go,bandwidth/bucket.go}`,
`cmd/node/main.go`, `pkg/coordinator/admission/assigner.go`,
`internal/api/handlers/config_admin.go`, `internal/config/{types.go,operator_yaml.go}`,
`cmd/coordinator/main.go`, `scripts/check_node_deps.sh`, `novactl` node tooling,
`docs/ROADMAP.md`, `docs/specs/{HEALING_PROTOCOL,FEDERATION_PROTOCOL,DATA_MODEL.sql,ARCHITECTURE_DECISIONS,THREAT_MODEL}.md`,
master federation design.

---

## Task 1 — Migration 0014: projection, reconcile queue, suppression, D8/D9, source binding (D-M5-2, D-M5-3, D-M5-8a)

**Files:** Create `internal/db/migrations/0014_liveness_healing.sql`,
`internal/db/queries/replication.sql`, `internal/db/queries/notify.sql`; Modify
`internal/db/queries/federation.sql`, `internal/db/migrations/MANIFEST.sha256`;
Test `internal/db/migrations/migrations_test.go`.

**SQL — DDL** (goose `-- +goose Up/Down/StatementBegin`):

```sql
ALTER TABLE nodes
  ADD COLUMN failure_domain_id    text,
  ADD COLUMN donor_principal_id   text,
  ADD COLUMN provider             text,
  ADD COLUMN asn                  text,
  ADD COLUMN region               text,
  ADD COLUMN operator_verified_at timestamptz,
  ADD COLUMN placement_weight     real NOT NULL DEFAULT 1.0
      CHECK (placement_weight >= 0.0 AND placement_weight <= 1.0),
  ADD COLUMN assignment_sync_state text NOT NULL DEFAULT 'current'
      CHECK (assignment_sync_state IN ('current','snapshot_required','reconciling')),
  ADD COLUMN revoked_signaled_at  timestamptz,
  -- D-M5-6-TEL scheduling hints (parallel to the existing last_free/stored_bytes; nodes is
  -- already UPDATEd on heartbeat, so telemetry lives here — NOT an in-memory memo).
  ADD COLUMN last_egress_remaining_bytes bigint CHECK (last_egress_remaining_bytes IS NULL OR last_egress_remaining_bytes >= 0),
  ADD COLUMN last_egress_capacity_bytes  bigint CHECK (last_egress_capacity_bytes  IS NULL OR last_egress_capacity_bytes  >= 0),
  ADD COLUMN last_egress_refill_bps      bigint CHECK (last_egress_refill_bps      IS NULL OR last_egress_refill_bps      >= 0);

-- source_node_id is a NULLABLE FK to nodes(id): NULL ⇒ coordinator-sourced. The reserved
-- synthetic wire.CoordinatorSourceID is NEVER stored here (it is not a nodes row); /pins/changes
-- translates NULL → ChangeSource.NodeID = wire.CoordinatorSourceID on the wire (D-M5-8a/8b).
-- source_attempts/source_next_attempt_at give durable requeue backoff for flapping sources (D-M5-8a).
ALTER TABLE pin_assignments
  ADD COLUMN source_node_id         uuid REFERENCES nodes(id),
  ADD COLUMN source_attempts        integer NOT NULL DEFAULT 0,
  ADD COLUMN source_next_attempt_at timestamptz;
ALTER TABLE pin_changes ADD COLUMN source_node_id uuid;

CREATE TABLE blob_replication_state (
  cid                    text PRIMARY KEY REFERENCES blobs(cid) ON DELETE CASCADE,
  healthy_acked_count    integer NOT NULL DEFAULT 0,
  sourceable_acked_count integer NOT NULL DEFAULT 0,   -- READ-sourceable (read-source/v1); see Task 2
  in_flight_count        integer NOT NULL DEFAULT 0,
  target_count           integer NOT NULL,
  safety_tier            text NOT NULL CHECK (safety_tier IN ('donor_lost','tier1','tier2','healthy')),
  local_recoverable      boolean NOT NULL DEFAULT false,
  durability_class       text NOT NULL CHECK (durability_class IN ('important','normal','cache')),
  dirty                  boolean NOT NULL DEFAULT false,
  updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX blob_replication_safety_idx     ON blob_replication_state (safety_tier, updated_at);
CREATE INDEX blob_replication_class_tier_idx ON blob_replication_state (durability_class, safety_tier);
CREATE INDEX blob_replication_dirty_idx      ON blob_replication_state (dirty) WHERE dirty;

CREATE TABLE blob_replication_reconcile_queue (
  cid         text PRIMARY KEY REFERENCES blobs(cid) ON DELETE CASCADE,
  reason      text NOT NULL,
  enqueued_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_suppression (
  event_type    text NOT NULL,
  destination   text NOT NULL,
  scope_key     text NOT NULL,
  last_fired_at timestamptz NOT NULL,
  PRIMARY KEY (event_type, destination, scope_key)
);
```

**SQL — backfill (anchor on `blob_storage_state`, not `pin_assignments`).** The
projection must include repair-eligible blobs with **zero** donor assignments —
those are exactly the `donor_lost` set M5 exists to surface. Anchor the seed on
`blob_storage_state` (M4.1's per-blob row, authoritative for `durability_class`),
LEFT JOIN holders:

```sql
INSERT INTO blob_replication_state
  (cid, healthy_acked_count, sourceable_acked_count, in_flight_count, target_count,
   safety_tier, local_recoverable, durability_class, dirty)
SELECT s.cid,
       0, 0, 0,
       CASE s.durability_class WHEN 'important' THEN 5 WHEN 'cache' THEN 2 ELSE 3 END,
       'donor_lost',                              -- recomputed authoritatively in Task 2
       (s.local_present AND s.local_role IN ('origin','staging','cache')),
       s.durability_class,
       true                                       -- seed dirty ⇒ Task 2 recompute owns the real counts
FROM blob_storage_state s
JOIN blobs b ON b.cid = s.cid
WHERE b.state IN ('active','quarantined')          -- repair-eligible (D-M5-RE)
ON CONFLICT (cid) DO NOTHING;
```

Seeding every row `dirty=true` lets Task 2's recompute establish the authoritative
counts/tier without duplicating the count logic in SQL here. Pre-M5 **pending**
`pin_assignments` keep NULL `source_node_id` (coordinator-sourced until the
scheduler rewrites them, D-M5-8a); **acked** rows may stay NULL.

**SQL — queries** (`replication.sql`): `UpsertReplicationState`,
`RecomputeReplicationStateRow :one` (the authoritative recompute SELECT, consumed
by Task 2), `MarkReplicationDirty(cids)`, `EnqueueReconcile(cid, reason)`,
`ListReconcileBatch(limit) :many`, `DeleteReconciled(cid)`,
`ListUnderReplicatedByTier(safety_tier, limit) :many`,
`RecomputeTargetsForClass(class, target)`. (`notify.sql`):
`GetSuppression :one`, `UpsertSuppression`. Extend `federation.sql`:
`GetPinSnapshotPage` to add `pa.source_node_id`; add `AssignPinWithSource` (Task 5).

- [ ] **Step 1 — failing test.** `TestMigration0014UpDownAndFrozen`
  (`migrations_test.go`): up → assert the three tables + the `nodes`/`pin_assignments`
  columns exist; assert a `blobs(state='active')` row with **no** `pin_assignments`
  produced a seeded `blob_replication_state` row (`safety_tier` present, `dirty=true`);
  down → assert all gone; assert `MANIFEST.sha256` covers `0014` and prior hashes
  are byte-identical. Run red: `go test ./internal/db/migrations -run 0014`.
- [ ] **Step 2 — write** `0014_*.sql` (DDL + backfill) + `replication.sql` +
  `notify.sql` + the `federation.sql` snapshot column; `make sqlc-generate`; append
  the `0014` hash to `MANIFEST.sha256`.
- [ ] **Step 3 — green.** `go test ./internal/db/... -run 0014`; `go build ./...`.
- [ ] **Step 4 — commit:**
  `feat(p2-m5): migration 0014 — blob_replication_state + reconcile/suppression + D8/D9 + source binding (P2-M5)`

## Task 2 — Projection maintenance: in-tx single-CID writers, per-CID lock, bounded drain, status-based read-sourceability (D-M5-2/2a/2b/2d)

**Files:** Create `internal/orchestrator/projection.go`; Modify
`internal/db/queries/{storage_state.sql,replication.sql}`,
`internal/federation/coordinator/assignments.go`; Test
`internal/orchestrator/projection_test.go`,
`internal/federation/coordinator/assignments_test.go`.

**Authoritative recompute (`RecomputeReplicationStateRow`)** — the projection is
rebuildable cache; this SELECT is its definition. Count **countable** holders only:

```sql
-- name: RecomputeReplicationStateRow :one
WITH holders AS (
  SELECT pa.state, n.status, n.assignment_sync_state, n.trust_state,
         (n.source_nebula_addr IS NOT NULL AND 'read-source/v1' = ANY(n.advertised_capabilities)) AS read_srcable
    FROM pin_assignments pa JOIN nodes n ON n.id = pa.node_id
   WHERE pa.cid = $1
)
SELECT
  count(*) FILTER (WHERE state='acked' AND status IN ('active','suspect') AND assignment_sync_state='current')                 AS healthy_acked,
  count(*) FILTER (WHERE state='acked' AND status IN ('active','suspect') AND assignment_sync_state='current'
                        AND trust_state<>'suspended' AND read_srcable)                                                          AS sourceable_acked,
  -- in_flight counts ONLY pending reservations on destinations still ELIGIBLE TO COMPLETE
  -- (D-M5-2 / Rev. 5 #5): a pending row on an unreachable/evicted/revoked/suspended node must
  -- not throttle healing forever. Liveness transitions also fail/clear such pending rows (Task 3).
  count(*) FILTER (WHERE state='pending' AND status IN ('active','suspect') AND trust_state<>'suspended')                       AS in_flight;
```

`sourceable_acked_count` is the **read**-sourceable count (status-based; the
`make_interval` time clause in `storage_state.sql.go:43,192` is **removed** —
D-M5-2a, status is the sole freshness authority). `safety_tier` is then derived in
Go (D-M5-2c): `0 healthy ⇒ donor_lost`; `1 ⇒ tier1`; `2..target-1 ⇒ tier2`;
`≥target ⇒ healthy`. `local_recoverable` = `blob_storage_state.local_present AND local_role IN
('origin','staging','cache') AND backend.Has(cid)` (D-M5-8b), looked up alongside.

**Go (`projection.go`):**

```go
// RecomputeCID recomputes one CID's projection row from authority and upserts it,
// under a per-CID advisory lock so admission, healing, ack/fail, and the dirty
// drain cannot compute from or write stale counts concurrently (D-M5-2d).
// The lock is orthogonal to AssignPin's global AcquireChangeLogLock (sequence
// ordering); this one is per-CID. Caller passes an open tx.
func RecomputeCID(ctx context.Context, tx pgx.Tx, cid string) (ReplState, error)

// lockCID takes pg_advisory_xact_lock(hashtext('replication:'||cid)); released at tx end.
func lockCID(ctx context.Context, tx pgx.Tx, cid string) error

// DrainReconcile recomputes up to `batch` queued CIDs (idempotent; each under its
// own short tx + per-CID lock) and deletes them from the queue. Returns count done.
func DrainReconcile(ctx context.Context, db Pool, batch int) (int, error)

// RecomputeTargets reassigns target_count + safety_tier for a class after an R
// change (D-M5-2b) and enqueues newly under-replicated CIDs.
func RecomputeTargets(ctx context.Context, db Pool, class string, target int) error
```

**Transaction boundaries.** Single-CID writers (`AssignPinWithSource`, `UnpinPin`,
the `/pins/{cid}/ack|fail` handlers) call `RecomputeCID` **inside their existing
mutation transaction**, after taking `lockCID`. Bulk liveness transitions (Task 3)
do **not** call `RecomputeCID` inline; they `MarkReplicationDirty` +
`EnqueueReconcile` in their tx, and `DrainReconcile` does the bounded async
recompute. `dirty=true` rows are recomputed before any scheduling read (Task 6
calls `RecomputeCID` for a dirty CID before reserving against it).

- [ ] **Step 1 — failing test.** `TestRecomputeCID_CountsCountableAckedOnly`
  (acked on active/suspect+`current` counts; on unreachable/evicted/revoked or
  `assignment_sync_state<>'current'` does not); `TestSafetyTierBoundaries`
  (0→donor_lost, 1→tier1, 2..target-1→tier2, ≥target→healthy);
  `TestReadSourceableRequiresCapAndStatus` (suspended/no-addr/no-read-cap excluded);
  `TestInFlightExcludesDeadDestinations` (a pending row on an unreachable/evicted/
  revoked/suspended node is **not** counted, so it cannot throttle healing forever);
  `TestLocalRecoverableComputed`; `TestRecomputeCIDPerCIDLockSerializes` (two
  concurrent recomputes don't interleave to a wrong count);
  `TestDrainReconcileBoundedIdempotent` (enqueue N, drain batch<N ⇒ ≤batch; re-run
  no-ops). Run red: `go test ./internal/orchestrator -run Projection`.
- [ ] **Step 2 — implement** `projection.go`; rewrite the sourceable query
  status-based (drop `make_interval`); wire single-CID writers in-tx with `lockCID`;
  `make sqlc-generate`.
- [ ] **Step 3 — green.** `go test ./internal/orchestrator ./internal/federation/coordinator`.
- [ ] **Step 4 — config-change recompute (D-M5-2b).** Implement `RecomputeTargets`;
  test `TestReplicationFactorChangeRecomputesTargetsAndTier`. Wire into the
  hot-reload hook in Task 9.
- [ ] **Step 5 — commit:**
  `feat(p2-m5): blob_replication_state maintenance — per-CID lock, bounded drain, status-based read-sourceability (P2-M5)`

## Task 3 — 5-state liveness sweeper + recovery + revocation fallout + endpoint matrix (D-M5-4/4a/4-REVOKE/-OBS/5)

**Files:** Create `internal/orchestrator/liveness.go`,
`internal/notify/notifier.go` (interface + `NoopNotifier`, so this task compiles
before Task 7's real emitter); Modify `internal/db/queries/federation.sql`,
`internal/federation/coordinator/{handlers.go,pins.go}`; Test
`internal/orchestrator/liveness_test.go`,
`internal/federation/coordinator/handlers_test.go`.

**Notifier seam (defined here, implemented in Task 7):**

```go
// internal/notify/notifier.go
type Event struct { Type, ScopeKey string; Payload map[string]any }
type Notifier interface { Emit(ctx context.Context, ev Event) }
type NoopNotifier struct{}
func (NoopNotifier) Emit(context.Context, Event) {}
```

**`assignment_sync_state` transition table (exact).** The coordinator sets it on
these events; countability (Task 2) requires `current`:

| Event | sets `assignment_sync_state` |
|---|---|
| `RegisterNode` (insert or ON CONFLICT) | `snapshot_required` (fresh desired set must be (re)synced) |
| heartbeat, was `active`/`suspect` | unchanged |
| heartbeat, `unreachable→active` | `reconciling` (had pending divergence) |
| `pins/changes` served, `since_seq` valid + reaches current head | `current` |
| `pins/changes` returns `snapshot_required` (cursor predates prune) | `snapshot_required` |
| `pins/snapshot` final page delivered (cursor exhausted at captured epoch) | `current` |
| `evicted` / `revoked` | left as-is (node is non-countable regardless) |

**Liveness sweeper (`reconcile_node_liveness`).** Applies FED v2 timers from
last_seen_at: `active→suspect` (missed `suspect_after_missed_heartbeats` ×
heartbeat interval), `suspect→unreachable` (`> unreachable_after_seconds`; healing
engages), `unreachable→evicted` (`> evicted_after_seconds`). On `→unreachable`,
`→evicted`, and revoke detection: select affected CIDs via the
`pin_assignments(node_id,state)` index, `MarkReplicationDirty` + `EnqueueReconcile`
in the transition tx (D-M5-2d). On all three, also **fail the destination's still-
`pending` reservations** for those CIDs (`UPDATE … SET state='failed'`) so a dead
in-flight reservation never blocks re-scheduling (Rev. 5 #5). `→evicted`
additionally **deletes** the node's remaining `pin_assignments` after enqueue
(D-M5-4a-EVICT: `pin_state` has no `retired`).
Revocation fallout (D-M5-4-REVOKE): rows are **retained** but excluded from counts
(the recompute already filters non-`active/suspect`); enqueue affected CIDs; for
`status='revoked' AND revoked_signaled_at IS NULL`, `Emit` `federation.node_revoked`
(scope `node_id`) and set `revoked_signaled_at` (D-M5-4-REVOKE-OBS).

**Heartbeat handler (`handlers.go:163` area), recovery transitions (D-M5-4a).**
Add the recovery transition (`suspect/unreachable→active` + sync-state per the
table) to `UpdateNodeHeartbeat`;
reject `evicted`/`revoked` (`evicted`→`registration_required`). `RegisterNode` ON
CONFLICT now sets `status='active'`, `assignment_sync_state='snapshot_required'`.
Heartbeat is the **canonical** liveness path — non-heartbeat requests may bump
`last_seen_at` but never change sync-state countability (D-M5-4a-LIVENESS-SIGNAL).
Endpoint matrix (D-M5-5) enforced on `pins/changes`, `ack`, `fail` in `pins.go`.

- [ ] **Step 1 — failing test.** `TestSweeperTimerTransitions`;
  `TestUnreachableEnqueuesAffectedCIDs`; `TestEvictedDeletesAssignmentsAfterEnqueue`;
  `TestRevokeRetainsRowsNonCountingEnqueues`; `TestRevokedSignaledExactlyOnce`
  (NoopNotifier records one call; second sweep no-ops);
  `TestHeartbeatReactivatesSetsReconcilingNotCountable`;
  `TestRegisterOnConflictSetsSnapshotRequired`;
  `TestSyncStateCurrentOnlyAtHeadOrSnapshotDone`; `TestEndpointStatusMatrix`
  (changes/ack/fail accept/reject per status). Run red:
  `go test ./internal/orchestrator ./internal/federation/coordinator -run Liveness`.
- [ ] **Step 2 — implement** `notifier.go` (interface+noop), `liveness.go`, handler
  + query edits; `make sqlc-generate`.
- [ ] **Step 3 — green;** `go build ./...`; `bash scripts/check_node_deps.sh`
  (notify must stay off the donor graph).
- [ ] **Step 4 — commit:**
  `feat(p2-m5): 5-state liveness sweeper + recovery/sync-state + revoke fallout + endpoint matrix (P2-M5)`

## Task 4 — Single placement engine over `assigner.go` (D-M5-7/7-CAP/7a/3a)

**Files:** Create `pkg/coordinator/admission/placement.go`; Modify
`pkg/coordinator/admission/assigner.go`, `internal/config/operator_yaml.go`; Test
`pkg/coordinator/admission/placement_test.go`.

**Go signatures:**

```go
type Candidate struct {
  NodeID                                 uuid.UUID
  FailureDomain, Principal, Provider,
  ASN, Region                            string   // "" ⇒ unknown bucket unless OperatorVerified
  OperatorVerified                       bool
  FreeBytes                              int64    // self-reported HINT only (D-M5-7-CAP)
  TrustState                             string   // probationary|trusted|suspended
  Reputation, PlacementWeight            float64
  Policy                                 PolicyFilters
}

// SelectDestination returns the best non-holder for (cid,class), or
// (uuid.Nil,false) when none qualifies. Anti-affinity is a PREFERENCE, never a veto.
func SelectDestination(cid string, class string, holders, candidates []Candidate,
                       repFloor float64) (uuid.UUID, bool)
```

**Destination eligibility (Rev. 5 #7).** A new-pin/repair **destination** must be
`status='active' AND assignment_sync_state='current' AND trust_state<>'suspended'`
(we do **not** deliver new assignments through a node mid-snapshot-recovery). The
caller (Task 6 scheduler / admission) passes only such `candidates`; the
`Candidate` struct's presence in the slice means "eligible destination."

**Algorithm.** (1) Exclude `suspended`, `reputation < repFloor`, capacity-infeasible
(`FreeBytes` hint), or `Policy` rejects. (2) `important` probation guard (T1.30): if
`len(holders) < 2`, exclude `probationary` candidates (never sole/second copy). (3)
**Domain key** for anti-affinity: for each dimension, use the value **only if
`OperatorVerified`**, else the literal `"unknown"`; two unverified candidates share
the `unknown` bucket and so are *not* mutually diverse (D-M5-3a). (4) Partition
candidates into those in a `failure_domain` (then principal/provider/asn/region)
**not already held**, preferring more-diverse; if none diverse has capacity, fall
back to best available (never veto). (5) Within the chosen partition, weight by
`~sqrt(FreeBytes) × TrustWeight × PlacementWeight` (bandwidth-decoupled) and pick
the max. `assigner.go` deletes its interim selection and calls `SelectDestination`.

**R validation (`operator_yaml.go`, D-M5-7a).** Validate all three factors `[1,20]`;
`important < 2` → **error**; `normal < 2` or `cache < 2` → **warn** appended to
`PrivacyWarnings`-style warnings (regenerable).

- [ ] **Step 1 — failing test.** `TestAntiAffinityPrefersDistinctVerifiedDomain`;
  `TestUnverifiedDomainsCollapseToUnknownNotDiverse`;
  `TestProbationaryNeverSoleOrSecondImportant`;
  `TestAntiAffinityNeverVetoesWhenNoDiverseCapacity`;
  `TestReputationFloorExcludes`; `TestWeightDecoupledFromBandwidth`
  (a high-free/low-trust node does not outrank a moderate-free/high-trust node by
  capacity alone); `TestImportantR1Errors_NormalR1Warns`. Run red:
  `go test ./pkg/coordinator/admission -run 'Placement|RValidation'`.
- [ ] **Step 2 — implement** `placement.go`; delegate from `assigner.go`; R
  validation in `operator_yaml.go`.
- [ ] **Step 3 — green;** `go build ./...`.
- [ ] **Step 4 — commit:**
  `feat(p2-m5): single placement engine — anti-affinity, unknown-collapse, trust caps, decoupled weight (P2-M5)`

## Task 5 — Donor↔donor repair: source-server generalization, token semantics, egress debit, late binding (D-M5-8/8a/8b/8c/8d/8e/RE)

**Files:** Modify `internal/federation/wire/{token.go,messages.go}`,
`internal/node/source/server.go`, `internal/federation/coordinator/{pins.go,assignments.go}`,
`internal/node/{agent/agent.go,agent/client.go,transfer/transfer.go}`,
`scripts/check_node_deps.sh`; Test `internal/node/source/server_test.go`,
`internal/federation/coordinator/pins_test.go`,
`internal/federation/wire/token_test.go`.

**Wire (`token.go`) — additive `Dest*` fields (D-M5-8e):**

```go
type Claims struct {
  JTI, AssignmentID string; Generation int64; CID, SourceNodeID, DestNodeID string
  NotBefore, NotAfter, MaxBytes int64; ProtocolVersion string
  DestAssignmentID string `json:"dest_assignment_id,omitempty"`  // NEW: destination's pending assignment
  DestGeneration   int64  `json:"dest_generation,omitempty"`     // NEW
}
```

`AssignmentID`/`Generation` **name the source donor's acked assignment** (the source
server verifies against its own `ProgressAckDelivered`, `server.go:173-176`);
`DestAssignmentID`/`DestGeneration` bind the destination's pending assignment.
`messages.go`: add `PinChange.Source *ChangeSource` and **`SnapshotItem.Source
*ChangeSource`** so snapshot recovery learns the source (D-M5-8a). Confirm `Claims`
still marshals deterministically with the omitempty fields (golden-vector test).

**Source server (`server.go`).** Accept the caller when
`IdentityFromCert.Role == RoleNode && claims.DestNodeID == requesterNodeID` (in
addition to the M4.1 `RoleCoordinator` path); `claims.SourceNodeID == s.nodeID`
unchanged. **Repair egress debit (D-M5-8d/8):** before the first body byte,
`s.budget.Take(size, now)` → on false, `refuse(503, budget_exceeded)`. **Oversize
hardening (D-M5-8d):** replace `io.Copy(w, io.LimitReader(rc, size+1))` with a copy
of **exactly** `size` (`io.CopyN(w, rc, size)`); if the source's pinned bytes exceed
`size` the re-import already failed earlier, so `CopyN` truncation cannot occur for
a correct blob. Repair-eligible (D-M5-RE): the donor serves what it has pinned; the
coordinator only mints grants for `active`/`quarantined` blobs (preflight stays
`blobs.state IN ('active','quarantined')`, separate from public read visibility).

**Coordinator mint + late binding (`pins.go`).** On `/pins/changes`, for a pending
assignment: **a NULL `pin_assignments.source_node_id` ⇒ coordinator-as-source** —
emit `ChangeSource.NodeID = wire.CoordinatorSourceID` (the synthetic id lives only
on the wire, never in the FK column, Rev. 5 #1) and mint a coordinator-source grant
(D-M5-8b emergency path also lands here). For a **non-NULL** `source_node_id`,
resolve the source's **current** `source_nebula_addr` and mint a grant: source claim
= the source's acked assignment (looked up), `DestAssignmentID`/`DestGeneration` =
this assignment's `assignment_id`/`generation`, `SourceNodeID=source`,
`DestNodeID=this node`. If the source is **not repair-sourceable now** (`status IN
('active','suspect')` + `assignment_sync_state='current'` + `repair-stream/v1`
advertised + addr present), **requeue/rewrite**: clear `source_node_id` (back to
NULL), `source_attempts = source_attempts + 1`,
`source_next_attempt_at = now() + backoff(source_attempts)`, and let the scheduler
(Task 6) re-pick once the backoff elapses (so a flapping source doesn't tight-loop).

**`AssignPinWithSource` (`assignments.go`) invariants.** Signature
`AssignPinWithSource(ctx, tx, cid string, dest, source uuid.UUID) (Assignment, error)`.
`source == uuid.Nil` ⇒ store **SQL NULL** (coordinator-sourced) — never the synthetic
`wire.CoordinatorSourceID`. Reject `source == dest` (`ErrSourceIsDest`). For a
non-Nil source, at reservation time it must be **repair-sourceable** and **acked**
for this CID; if not, return `ErrSourceNotSourceable` (caller re-picks). Writes
`pin_assignments.source_node_id` (NULL or the FK) + copies into
`pin_changes.source_node_id`, under the existing global `AcquireChangeLogLock`
(sequence order) **and** `lockCID` (per-CID, Task 2), then `RecomputeCID` in-tx.

**`repair-stream/v1` (D-M5-8c).** Donor `agent` advertises `wire.CapRepairStream`;
the coordinator selects a **donor** source only from advertisers (a non-advertiser
can still be a coordinator-as-source destination). Mixed-version: non-advertisers
are read-sourceable but not repair-sourceable.

**Donor transfer (`transfer.go`, `agent/client.go`).** When an `assign` change's
`source.node_id` is another donor, the destination donor **verifies the grant's
destination binding before fetching (Rev. 5 #4):** `token.DestNodeID == own node
id`, `token.DestAssignmentID == PinChange.AssignmentID`, and
`token.DestGeneration == PinChange.Generation`. A mismatch, or a **malformed partial**
binding (`DestAssignmentID` set but `DestGeneration == 0`, or vice-versa), is a
**refusal before fetch** (no `ack`/`fail` ambiguity — it never started). On a match
it fetches from `source.nebula_addr` (not the coordinator) under `source.token`; the
rest of the M4 loop (reimport→root-CID verify→pin→persist→ack) is unchanged; `ack`
uses **this donor's** (destination) `assignment_id`/`generation`.

- [ ] **Step 1 — failing test.**
  `TestClaimsMarshalsDeterministicallyWithDestFields` (golden vector unchanged when
  Dest* empty); `TestSourceServesRepairToMatchingDest`,
  `TestSourceRefusesMismatchedDest` (`source_unauthorized`),
  `TestSourceRefusesSourceAssignmentMismatch` (token source-assignment ≠ local
  progress ⇒ `progress_mismatch`); `TestSourceStreamsExactlySizeNoOverwrite`;
  `TestSourceShortReadDestNoAck` (a truncated/corrupt source body fails the dest's
  re-import CID verify ⇒ **no ack**, Rev. 5 #10);
  `TestRepairServeDebitsEgressRefusesOverBudget`;
  `TestDestVerifiesDestBindingBeforeFetch` (`DestAssignmentID`/`DestGeneration` ≠
  `PinChange` ⇒ refuse pre-fetch, no ack/fail) and
  `TestMalformedPartialDestFieldsRefused` (one of `DestAssignmentID`/`DestGeneration`
  set, the other zero ⇒ refuse, Rev. 5 #4);
  `TestLateMintRequeuesWithBackoffWhenSourceNotRepairSourceable`
  (`source_attempts`++ and `source_next_attempt_at` set);
  `TestAssignPinWithSourceRejectsSourceEqualsDest`;
  `TestAssignPinWithSourceNilStoresSQLNull` and `TestPinsChangesNullSourceEmitsCoordinatorSourceID`
  (Rev. 5 #1 — NULL in DB, synthetic id only on the wire);
  `TestSnapshotItemCarriesSource`;
  `TestMixedVersionDonorReadSourceableNotRepairSourceable`;
  `TestCoordinatorSourceIDEmergencyPath`. Run red:
  `go test ./internal/node/source ./internal/federation/coordinator ./internal/federation/wire -run 'Repair|Source|Claims'`.
- [ ] **Step 2 — implement** wire/server/pins/assignments/agent/transfer; `make
  sqlc-generate`; add the reviewed `donor-deps-boundary` allowlist entries.
- [ ] **Step 3 — green;** `go build ./...`; `bash scripts/check_node_deps.sh`.
- [ ] **Step 4 — commit:**
  `feat(p2-m5): donor↔donor repair — source-assignment-bound grants, egress debit, repair-stream/v1, late binding (P2-M5)`

## Task 6 — Healing orchestrator tick loop + telemetry-hinted pacing + warn-not-force R (D-M5-6/6-TEL/12)

**Files:** Create `internal/orchestrator/{orchestrator.go,scheduler.go}`; Modify
`internal/federation/wire/messages.go`, `internal/federation/coordinator/handlers.go`,
`internal/node/agent/agent.go`, `internal/node/bandwidth/bucket.go`,
`cmd/coordinator/main.go`; Test `internal/orchestrator/scheduler_test.go`.

**Heartbeat telemetry (D-M5-6-TEL).** `HeartbeatRequest` gains optional
`EgressBudgetRemainingBytes`, `EgressBudgetCapacityBytes`,
`EgressRefillBytesPerSecond` (omitempty). `bandwidth.Bucket` gains read-only
`Remaining(now) int64` / `Capacity() int64` / `RefillPerSecond() int64`; the agent
reports them. The coordinator persists them on the **`nodes` columns added in
Task 1** (`last_egress_remaining_bytes`/`_capacity_bytes`/`_refill_bps`, written by
the same heartbeat UPDATE as `last_free_bytes` — **not** an in-memory memo, Rev. 5
#8) and uses them **only** as the `step_capacity` hint —
`step_capacity = min(reported_remaining, link_speed × step_seconds)`. The donor
`Bucket.Take` stays authoritative; an over-optimistic hint still yields a donor
`budget_exceeded` refusal.

**Scheduler tick (`scheduler.go`).** Per tick: (1) `DrainReconcile(batch)`
(bounded). (2) `ListUnderReplicatedByTier('tier1', N)`; for each, `RecomputeCID`
under `lockCID` **first** (never schedule from a stale/dirty row, D-M5-2d), then if
still `tier1` select a source (asymmetric: max `step_capacity × reputation` over
**repair-sourceable** holders, byte-feasible) + destination (Task 4 engine, eligible
candidates only — active+current+non-suspended) + `AssignPinWithSource` + mint
grant. Skip any pending assignment whose `source_next_attempt_at > now()` (Rev. 5
#3 backoff) so a flapping source isn't re-picked before its backoff elapses. (3) **Strict Tier-1:** only if Tier-1 is empty
this tick, process `tier2` toward `target_count`. (4) Restart re-derives tiers from
the projection (no persisted queue). Emit warn-not-force when
`replication.factor.important < 5` at startup/reload alongside `PrivacyWarnings`
(D-M5-12).

**`orchestrator.go`** runs `reconcile_node_liveness` + `DrainReconcile` +
`scheduler.Tick` + the Task-8 metrics on `tick_interval_seconds`, single-leader; a
`context` cancel stops it; `cmd/coordinator` starts it after DB+federation are up.

- [ ] **Step 1 — failing test.** `TestStrictTier1BeforeTier2`
  (Tier-2 untouched while Tier-1 non-empty); `TestPendingDoesNotLiftTier1`;
  `TestSourceSelectionMaxCapacityReputationRepairSourceableOnly`;
  `TestSchedulerRecomputesDirtyBeforeReserving`;
  `TestStepCapacityHintDonorStillRefuses` (hint says ok, donor bucket refuses);
  `TestRestartRederivesTiersFromProjection`; `TestImportantBelowFiveWarns`. Run red:
  `go test ./internal/orchestrator -run Scheduler`.
- [ ] **Step 2 — implement** scheduler/orchestrator + telemetry wiring; run from
  `cmd/coordinator`; `make sqlc-generate`.
- [ ] **Step 3 — green;** `go build ./...`; `bash scripts/check_node_deps.sh`.
- [ ] **Step 4 — commit:**
  `feat(p2-m5): healing orchestrator — strict Tier-1, repair-sourceable selection, telemetry-hinted pacing (P2-M5)`

## Task 7 — Webhook dispatcher: best-effort signed emitter + scoped suppression (D-M5-9/9a)

**Files:** Create `internal/notify/{emitter.go,suppress.go}`; Modify
`cmd/coordinator/main.go` (inject the real `Notifier`); Test
`internal/notify/{emitter_test.go,suppress_test.go}`.

**Signing wire format (concrete, D-M5-9).** When `WebhookDestination.Secret`
(`secret_file`) is set, sign with HMAC-SHA256:

- Header `X-Nova-Webhook-Timestamp: <unix seconds>`.
- Header `X-Nova-Webhook-Signature: v1=<hex(HMAC_SHA256(secret, signedString))>`,
  where `signedString = timestamp + "." + rawRequestBody` (body canonicalized as the
  exact JSON bytes sent).
- Header `Content-Type: application/json`. No signature headers when no secret.
- The receiver's replay window is documentation-only (we sign+timestamp; receivers
  reject stale timestamps); the emitter does not retain state for replay.

**Emitter (`emitter.go`).** `BestEffortHTTP` implements `Notifier`: per-destination
`http.Client{Timeout}`; one POST per matching destination (`WebhookDestination.Events`
filter); on error, slog `warn` + increment a counter-shaped field
(`nova_webhook_delivery_failures_total`-named log field); **no** retry/outbox.
Honors paranoid gating (skip emit entirely when `paranoid=true`). `Emit` is
non-blocking for the caller path but **bounded**: deliveries run through a small
fixed **worker pool / semaphore** (e.g. cap 4 concurrent POSTs) so an event storm
cannot fan out unbounded goroutines; if the pool is saturated the event is dropped
with a slog `warn` (webhooks are best-effort notifications). `Emit` can never panic
into the healing loop (recover at the boundary).

**Suppression (`suppress.go`, D-M5-9a).** Before emitting, `GetSuppression(type,
dest, scope_key)`; if `now - last_fired_at < window(type)`, skip; else emit +
`UpsertSuppression`. `scope_key`: `node_revoked`→node_id; `degraded`→`"global"`;
`shrinking`→limiting_class; `concentrated`→`dim+":"+value`; `homogeneous`→dim.
Windows: `degraded`/`concentrated`/`homogeneous` = `mass_casualty_window`;
`shrinking` = 24h; `node_revoked` = 0 (always, but dedup on `revoked_signaled_at`
upstream). Durable (the Task-1 table) so once-per-window survives restart.

- [ ] **Step 1 — failing test.** `TestBestEffortPostTimeoutNoCascade` (slow/500
  destination returns promptly, logs, no panic/block);
  `TestHMACSignatureFormatV1` (header value + signed string exact);
  `TestNoSignatureWhenSecretUnset`; `TestParanoidSkipsEmit`;
  `TestScopedSuppressionNodeADoesNotSuppressNodeB`;
  `TestSuppressionDurableAcrossRestart`; `TestEventsFilterPerDestination`;
  `TestBoundedConcurrencyUnderStorm` (N≫pool events ⇒ ≤pool concurrent POSTs,
  excess dropped-with-warn, no goroutine blowup, Rev. 5 #9). Run red:
  `go test ./internal/notify`.
- [ ] **Step 2 — implement** emitter + suppression over the Task-1
  `webhook_suppression` table; `make sqlc-generate`; inject into `cmd/coordinator`.
- [ ] **Step 3 — green;** `go build ./...`.
- [ ] **Step 4 — commit:**
  `feat(p2-m5): best-effort HMAC-signed webhook dispatcher + durable scoped suppression (P2-M5)`

## Task 8 — Concentration metrics + mass-casualty + slow-attrition runway (D-M5-10/11)

**Files:** Create `internal/orchestrator/{concentration.go,runway.go}`; Modify
`internal/orchestrator/orchestrator.go`; Test
`internal/orchestrator/{concentration_test.go,runway_test.go}`.

**Concentration (`concentration.go`, D-M5-10).** Bounded periodic read over
`pin_assignments(state='acked')` ⨝ `nodes`. Per-node pin-incidence **Gini**
(`G = Σ_i Σ_j |x_i - x_j| / (2 n Σ x)`, 0 for ≤1 node). Per dimension
(`donor_principal`/`failure_domain`/`provider`/`asn`/`region`): collapse
unverified/NULL to `"unknown"` **before** grouping (D-M5-3a/10); compute
`largest_share`, `top_k_share` (k from config, clamp `k ≤ #groups`),
`normalized_entropy = H/ln(#groups)` with `#groups==1 ⇒ 0` (avoid `ln(1)=0`
divide). Emit `federation.concentrated` (largest_share over threshold) /
`federation.homogeneous` (normalized_entropy under threshold) via the Notifier.
Edge cases tested: zero pins, one node, all-`unknown`, single group, `k>#groups`.

**Runway (`runway.go`, D-M5-11).** Mass-casualty: count `active→unreachable` in
`mass_casualty_window_seconds`; if `> ratio × active_at_window_start`,
`federation.degraded` (scope `global`, no budget override). Slow-attrition per
class: `desired = Σ corpus_bytes_c × R_c`;
`repair_time_days = desired / surviving_daily_egress`;
`storage_headroom = surviving_free_donor_bytes / projected_required_replica_bytes`;
`active_node_trend = now vs trailing-28d`. If `repair_time_days > floor` **or**
`storage_headroom < 1`, emit `federation.shrinking` (scope `limiting_class`) with
all three fields. Never call it `runway_days`.

- [ ] **Step 1 — failing test.** `TestGiniEdgeCases` (zero/one/uniform/skewed);
  `TestEntropyNormalizationSingleGroupZero`; `TestTopKClampedToGroupCount`;
  `TestUnknownCollapsedBeforeMetrics`; `TestConcentratedAndHomogeneousFire`;
  `TestMassCasualtyDegradedNoOverride`;
  `TestShrinkingUsesRepairTimeAndStorageHeadroom`. Run red:
  `go test ./internal/orchestrator -run 'Concentration|Runway'`.
- [ ] **Step 2 — implement** concentration + runway; wire into the tick loop.
- [ ] **Step 3 — green;** `go build ./...`.
- [ ] **Step 4 — commit:**
  `feat(p2-m5): concentration (Gini/entropy) + mass-casualty + corrected slow-attrition metrics (P2-M5)`

## Task 9 — Config, `/settings`, novactl, observability (D-M5-2a/7a/13)

**Files:** Modify `internal/config/{types.go,operator_yaml.go}`,
`internal/api/handlers/config_admin.go`, `novactl` node tooling; Test
`internal/config/operator_yaml_test.go`,
`internal/api/handlers/config_admin_test.go`.

Wire the FED liveness timers (`suspect_after_missed_heartbeats`,
`unreachable_after_seconds`, `evicted_after_seconds`) + healing/placement knobs
(`reputation_floor`, selection mode) into validation + hot-reload; the R-change
reload hook calls `RecomputeTargets` (Task 2). **Deprecate `prune_stale_seconds`**
(D-M5-2a): mark it accepted-but-ignored with a one-time startup warn (status is
freshness authority). First-class `/settings`: `replication.factor.*`,
`mass_casualty_threshold_ratio`, `capacity_runway_floor_days`, `reputation_floor`;
liveness timers + selection internals stay advanced/defaulted (`fieldEffect`
classification). `novactl node set-domain <id> --failure-domain/--principal/
--provider/--asn/--region` (DB-direct; sets `operator_verified_at=now()`). Emit the
D-M5-13 slog set (`orchestrator.tick.*`/`liveness.transition.*`/`heal.*`/`repair.*`/
`fed.concentration.*`/`federation.*`).

- [ ] **Step 1 — failing test.** `TestRejectImportantR1AcceptNormalR1WithWarn`;
  `TestPruneStaleSecondsDeprecatedWarn`; `TestNovactlSetDomainSetsVerifiedAt`;
  `TestSettingsExposesFirstClassKnobsOnly`; `TestRChangeReloadRecomputesTargets`.
  Run red: `go test ./internal/config ./internal/api/handlers -run 'M5Config'`.
- [ ] **Step 2 — implement** config/validation/`/settings`/novactl + slog fields.
- [ ] **Step 3 — green;** `go build ./...`.
- [ ] **Step 4 — commit:**
  `feat(p2-m5): config validation + /settings knobs + novactl domain tooling + observability (P2-M5)`

## Task 10 — Functional chaos E2E + spec amendments + ROADMAP (D-M5-14, exit criterion)

**Files:** Create `internal/federation/e2e/m5_healing_test.go` (extend the M4.1
loopback-mTLS harness); Modify
`docs/specs/{HEALING_PROTOCOL,FEDERATION_PROTOCOL,DATA_MODEL.sql,ARCHITECTURE_DECISIONS,THREAT_MODEL}.md`,
the master federation design, `docs/ROADMAP.md`.

**Fixture topology (exact).** One coordinator + **four** donors over loopback-mTLS
from the start (no mid-test registration — Rev. 5 #6), all advertising
`read-source/v1` + `repair-stream/v1`, in **two** operator-verified failure domains:
`fdA={donor1,donor2}`, `fdB={donor3,donor4}`. One `important` blob (R=3) uploaded
and replicated+acked on **donor1,donor2,donor3** ⇒
`blob_replication_state{healthy_acked=3, safety_tier='healthy'}`; **donor4 is spare
repair capacity** (holds nothing initially). Egress budgets sized so a single repair
fits but a forced over-budget serve refuses.

**Scenario assertions (the design exit criterion):**

1. **Kill donor3** (close its listener). The sweeper transitions
   `active→suspect→unreachable` (drive the clock); only this blob's row is
   recomputed (`healthy_acked=2`, `tier2`). Assert no full-table scan (the affected
   set came from the `pin_assignments(node_id,state)` index), and that donor3's
   still-`pending` rows (if any) are failed (Rev. 5 #5).
2. **Heal — deterministic.** The orchestrator selects donor1 or donor2 as source
   (repair-sourceable, max capacity×rep) and **donor4** as destination — donor4 is
   the eligible (`active`+`current`) non-holder, and anti-affinity prefers it
   (`fdB`, distinct from the surviving `fdA` holders). Assert the reservation is
   `AssignPinWithSource(cid, dest=donor4, source=donorX)` with a non-NULL FK source.
3. **Donor↔donor repair.** donor4 verifies the grant's destination binding
   (`DestAssignmentID`/`DestGeneration` == its `PinChange`) **before** fetching, then
   fetches the **ciphertext envelope** from the source donor under a
   coordinator-minted grant whose `AssignmentID` = the **source's** acked assignment
   and `DestAssignmentID` = donor4's pending assignment; the source donor's egress
   budget is **debited**; donor4 verifies by re-import + root-CID before ack; the row
   returns to `healthy`. Assert an over-budget serve is refused (`budget_exceeded`)
   and donor4 does **not** ack; assert a short/corrupt source body also yields **no
   ack** (Rev. 5 #10).
4. **Recovery.** Restart donor3; on heartbeat it flips `unreachable→active` with
   `assignment_sync_state='reconciling'` and is **not** counted until it reaches
   the current head; assert a snapshot-recovery path delivers `SnapshotItem.source`.
5. **Eviction / revocation.** Advance the clock past `evicted_after_seconds` for a
   never-returning donor ⇒ its `pin_assignments` deleted + CIDs enqueued; `novactl
   node revoke donor2` (DB-direct) ⇒ rows retained non-counting, CIDs enqueued,
   exactly one `federation.node_revoked{scope=donor2}` captured by a test Notifier.
6. **Signals.** A 2-of-3 simultaneous kill trips `federation.degraded` once per
   window (no budget override); a corpus/budget fixture below floor trips
   `federation.shrinking` with `repair_time_days`/`storage_headroom`; a fixture
   with all pins in `fdA` trips `federation.concentrated{scope=failure_domain:fdA}`;
   assert node A's revoke suppression key does not suppress node B's.
7. **Invariants.** No coordinator cache row counted toward `healthy_acked`; the
   coordinator served no unverified bytes; no donor exceeded budget;
   `donor-deps-boundary` + `migrations-frozen` green; a `donor_lost ∧
   ¬local_recoverable` fixture CID is not scheduled.

**Spec edits (D-M5-14):** FED (donor↔donor live; `repair-stream/v1` transition;
heartbeat egress telemetry; snapshot `source`; the source-assignment token
semantics); HEAL (cache≠replica; repair-source=donors; placement-weight
calibration; **runway formula/label fix**); DATA_MODEL (`blob_replication_state`,
reconcile + suppression tables, D8/D9 columns, `pin_assignments.source_node_id`
(nullable FK) + `source_attempts`/`source_next_attempt_at`,
`nodes.assignment_sync_state` + the `last_egress_*` telemetry columns); THREAT_MODEL (code-enforced Sybil/repair-replay);
**`ARCHITECTURE_DECISIONS.md:161` `important` default `3 → 5`**. Mark ROADMAP P2-M5
done.

- [ ] **Step 1 — failing E2E** asserting the full chain (red until Tasks 1–9 land;
  run last). Run: `go test ./internal/federation/e2e -run M5Healing`.
- [ ] **Step 2 — green** the E2E; apply the spec amendments + drift fix.
- [ ] **Step 3 — full suite.** `go test ./...`; `bash scripts/check_node_deps.sh`;
  `go test ./internal/db/migrations/...` (frozen); gofmt touched files.
- [ ] **Step 4 — commit (test):**
  `test(p2-m5): e2e liveness→Tier-1 heal→donor↔donor repair→recovery + signals (P2-M5)`
- [ ] **Step 5 — commit (docs):**
  `docs(p2-m5): mark P2-M5 done; HEAL/FED/DATA_MODEL/THREAT amendments + ARCH R-default fix (P2-M5)`

## Exit criterion

The Task-10 E2E passes end to end: a killed donor transitions through the 5-state
machine; the projection updates only affected CIDs; the orchestrator restores
Tier-1 via budget-respecting donor↔donor repair (source-assignment-bound grant,
source donor debited, no budget override, destination verifies before ack); a
returned node does not count until it reconciles to the current epoch, and a
long-offline node recovers its source via snapshot; an evicted node's assignments
are deleted while a revoked node's are retained-non-counting with a single
`federation.node_revoked`; provider-purge, slow-attrition, and concentration emit
the correct webhooks once per scoped window; cache/origin never counts toward `R`;
and `donor-deps-boundary` + `migrations-frozen` stay green. Then: branch
`p2-m5-liveness-healing`, local fast-forward + annotated tag
`p2-m5-liveness-healing`, no remote push.
