# P2-M7 Production Hardening & Donor Release — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task.
> Steps use checkbox (`- [ ]`) syntax for tracking. Milestone workflow: all work on
> the feature branch `p2-m7-production-hardening-release`; finish with a **local
> fast-forward merge to `main` + annotated tag `p2-m7-production-hardening-release`;
> no remote push.**

**Goal:** Ship the volunteer-ready federation release: Prometheus `/metrics`
(coordinator-only), the corpus-scale benchmark gate, N−1/mixed-version compat tests,
operational drills with runbooks, the `drain`/`undrain` lifecycle primitive, and the
final volunteer docs — no new replication policy.

**Architecture:** Everything lands coordinator-side (plus docs). One forward-only
migration (`0016`, `nodes.draining_at`); query edits split rigidly into safety-count
vs placement vs selection; a new `internal/metrics` package on its own listener with
hook-based instrumentation seams; a `internal/benchcorpus` test-gated bench harness
over the existing `dbtest` testcontainers substrate; drills extend the existing
orchestrator/e2e test patterns.

**Tech Stack:** Go 1.26, PostgreSQL + sqlc (`make sqlc-generate`),
`prometheus/client_golang` (already in `go.mod` as indirect — promote to direct),
goose migrations (frozen manifest), testcontainers (`internal/dbtest`), bash for the
cross-version script.

**Design doc:**
[`../../specs/phase2/2026-07-01-phase2-m7-production-hardening-release-design.md`](../../specs/phase2/2026-07-01-phase2-m7-production-hardening-release-design.md)
(decisions D-M7-1 … D-M7-8).

## Global Constraints

Non-negotiable (ratified with Bug, 2026-07-03 review):

- **Drain is not P2-M6.1.** Drain stays node-scoped, operator-initiated, one-shot,
  non-hysteretic. No reputation-triggered scan, no below-floor replacement queue, no
  background policy loop.
- **`draining_at` is authoritative.** `placement_weight = 0` stays semantically
  distinct from drain; explicit tests prove weight-zero-but-not-draining still
  counts toward durability, and draining-with-weight>0 does not.
- **Safety counts exclude draining; selection may include draining.** Count queries
  and selection queries are edited in separate, separately-tested steps.
- **Cross-version tests never require old-coordinator drain.** All pairings prove
  `join → serve → audit`; drain/decommission is HEAD-coordinator-only.
- **Metrics have bounded labels + a scrape test** that rejects `cid`, `blob`,
  `path`, `filename`, `url`, `collection` as label names.
- **Benchmark is scratch-DB-guarded**: refuses a non-scratch DSN without an explicit
  override; testcontainers default is inherently scratch.
- **Cosign values come from the actual workflow** (`.github/workflows/ci.yml`
  `donor-sbom-sign`); no invented identity/issuer strings, no placeholders.
- **Undrain is part of the drain lifecycle, kept tiny**: clear `draining_at`,
  enqueue the node's CIDs for recompute; heartbeat/re-register must NOT clear drain.

House-wide invariants:

- Donor dependency boundary: `scripts/check_node_deps.sh` stays green (and gains the
  explicit prometheus deny, Task 6). **No new donor code in this milestone.**
- Shipped migrations are frozen: never edit `internal/db/migrations/0001–0015`;
  `0016` is a new forward-only file appended to `MANIFEST.sha256`
  (`(cd internal/db/migrations && sha256sum 0016_node_draining.sql >> MANIFEST.sha256)`).
- After ANY `internal/db/queries/*.sql` change: `make sqlc-generate`, commit the
  regenerated `internal/db/gen/` alongside; `make codegen-check` must pass.
- `gofmt` only files you touched. golangci-lint is CI-only.
- Commit messages: `feat(p2-m7): <summary> (P2-M7)` / `docs(p2-m7): …` /
  `test(p2-m7): …`, each ending with the trailer
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- DB-touching tests use `internal/dbtest` (testcontainers) like their neighbors.

## Preconditions (verified during exploration; names are real)

- `nodes` has `status` (`active|suspect|unreachable|evicted|revoked`),
  `trust_state`, `reputation_score`, `placement_weight` (0.0–1.0, `0014`),
  `assignment_sync_state`, `advertised_capabilities`, `source_nebula_addr`,
  `last_seen_at`. **No `draining_at` yet.**
- `RecomputeReplicationCounts` (`internal/db/queries/replication.sql`) computes
  `healthy_acked` / `sourceable_acked` / `in_flight` from a `holders` CTE gated on
  `status IN ('active','suspect')` + `assignment_sync_state='current'`.
- `CountSourceableHolders` + `ListSourceableHolders`
  (`internal/db/queries/storage_state.sql`): the count feeds commit/prune/read
  safety; the list ends `ORDER BY n.reputation_score DESC, n.id`.
- `ListRepairSourceHolders` (`replication.sql`) ends
  `ORDER BY (COALESCE(n.last_egress_remaining_bytes,0)::float8 * n.reputation_score) DESC, n.reputation_score DESC, n.id LIMIT 1`;
  `IsRepairSourceableForCID` / `GetRepairSource` (`federation.sql`) are
  reservation-time guards — draining nodes must REMAIN eligible there (no edit).
- `ListPlacementCandidates` (`replication.sql`) filters
  `status='active' AND assignment_sync_state='current' AND trust_state<>'suspended'`.
- Node-scoped bulk fallout already exists (`federation.sql`):
  `FailNodePendingAssignments` (pending→failed), `MarkReplicationDirtyForNode`,
  `EnqueueReconcileForNode(reason, node_id)` (reconcile-queue upsert). The liveness
  sweeper (`internal/orchestrator/liveness.go` `enqueueNodeCIDs`) is the usage
  pattern to mirror.
- `RegisterNode` (`federation.sql`) `ON CONFLICT (id) DO UPDATE` re-activates and
  refreshes many columns — it must NOT gain `draining_at`.
- Register handler (`internal/federation/coordinator/handlers.go`): checks `fed/v1`
  in `SupportedProtocols` → `wire.NegotiateCapabilities(req.Capabilities,
  s.cfg.RequiredCapabilities)` → `missing_capability`. `ServerConfig
  .RequiredCapabilities []string` (`server.go`) is `[]` in M2.
- Capability constants (`internal/federation/wire/messages.go`): `CapPinChangeLog`,
  `CapSnapshot`, `CapBlobTransfer`, `CapRepairStream`, `CapReadSource`,
  `CapAuditBlockHash`. Fail reasons incl. `FailReasonOutOfSpace = "out_of_space"` +
  `NormalizeFailReason`.
- `novactl` node commands: `cmd/novactl/node.go` (subcommand switch) +
  `node_db.go` (`withNodeDB`, `withNodeDBPool`, `parsePGUUID`, `revokeNode` core +
  `cmdNodeRevoke` — the exact pattern `drain`/`undrain` mirror).
- Config: `internal/config/types.go` `Coordinator` struct (tri-state pointer
  precedent: `RecordSourceIP *bool`); `PossessionAudit` block with
  `base_interval_seconds` / `deadline_seconds` (cross-version script shortens
  cadence via these). Validation entry: `validate(cfg)` in `operator_yaml.go`.
- `cmd/coordinator/main.go`: orchestrator wired ~line 487–560, possession scheduler
  ~line 565–575; signal-driven lifecycle via `signal.NotifyContext`.
- `go.mod`: `github.com/prometheus/client_golang v1.23.2 // indirect` (promote).
- Existing metric-ish queries (`internal/db/queries/metrics.sql`):
  `ListAckedNodeDimensions`, `SumCorpusBytesByClass`, `SurvivingCapacity`,
  `CountRecentlyUnreachable`.
- Bench substrate: `internal/dbtest` (Postgres testcontainers), migration tests in
  `internal/db/migrations/*_test.go` (e.g. `possession_state_test.go`) are the
  pattern for the `0016` behavior test.
- Prior milestone tag exists: `p2-m6-possession-audits` (verified via `git tag`).
- CI: `.github/workflows/ci.yml` has `test`, `donor-deps-boundary`, `donor-build`,
  `donor-sbom-sign` (cosign keyless: `cosign sign --yes
  ghcr.io/nova-archive/nova-node@<digest>`, `actions/attest-build-provenance@v1`,
  `cosign attest --yes --type spdxjson`), `coordinator-sbom-sign`, `govulncheck`.

## File structure

**New files:**

- `internal/db/migrations/0016_node_draining.sql` — `nodes.draining_at` + partial index.
- `internal/db/migrations/draining_state_test.go` — migration behavior + upgrade test.
- `internal/db/queries/drain.sql` — drain marker + drain-debt + below-floor-debt queries.
- `cmd/novactl/node_drain.go` — `drain`/`undrain` subcommands (cores + CLI).
- `cmd/novactl/node_drain_test.go` — core tests (refusals, idempotency, fallout).
- `internal/metrics/metrics.go` — registry, process-local instruments, hook methods.
- `internal/metrics/collector.go` — DB-derived scrape-time collector.
- `internal/metrics/server.go` — listener helper (bind + serve + shutdown).
- `internal/metrics/metrics_test.go` — families, label discipline, plane isolation.
- `internal/benchcorpus/seed.go` — skewed corpus seeder.
- `internal/benchcorpus/bench_test.go` — env-gated corpus bench + artifact writer.
- `internal/benchcorpus/explain_test.go` — deterministic query-plan gate.
- `internal/federation/coordinator/compat_matrix_test.go` — capability matrix.
- `internal/orchestrator/drain_test.go` — drain countability/placement/selection drills.
- `internal/orchestrator/drills_test.go` — revocation + provider-loss drills.
- `internal/node/transfer/outofspace_test.go` — disk-full error-path drill.
- `internal/node/state/corrupt_test.go` — corrupt-state fail-safe drill.
- `internal/federation/e2e/m7_drain_test.go` — drain e2e capstone (repair FROM a draining source).
- `scripts/crossversion_e2e.sh` — N−1 mixed-binary matrix runner.
- `reports/benchmarks/.gitkeep` — artifact directory.
- `docs/runbooks/donor-lifecycle.md` — revoke/suspend/drain/below-floor runbooks.
- `docs/runbooks/failure-drills.md` — provider-loss / disk-full / corrupt-state runbooks.

**Modified files:**

- `internal/db/queries/replication.sql` — `RecomputeReplicationCounts`,
  `ListPlacementCandidates`, `ListRepairSourceHolders`.
- `internal/db/queries/storage_state.sql` — `CountSourceableHolders`,
  `ListSourceableHolders`.
- `internal/db/migrations/MANIFEST.sha256` — append `0016`.
- `internal/db/gen/*` — regenerated (never hand-edited).
- `cmd/novactl/node.go` — add `drain`/`undrain` to the subcommand switch + usage.
- `internal/config/types.go` + `internal/config/operator_yaml.go` —
  `metrics_listen_addr` (tri-state) + validation.
- `cmd/coordinator/main.go` — metrics listener wiring (startup-fatal bind) + hooks.
- `internal/federation/coordinator/server.go` / `handlers.go` —
  `OnRegisterFailure` hook (2 lines each).
- `internal/audit/possession/verify.go` / `trust.go` — observer hook calls.
- `pkg/coordinator/storage/readsource.go` — fetch/selection observer hook calls.
- `scripts/check_node_deps.sh` — explicit `github.com/prometheus/*` deny.
- `Makefile` — `bench-corpus`, `bench-corpus-ci`, `bench-corpus-explain`,
  `crossversion-e2e` targets.
- `.github/workflows/ci.yml` — `bench-regression` job.
- `go.mod` / `go.sum` — client_golang → direct.
- `docs/quickstart/donor.md`, `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`,
  `docs/VERSIONING.md`, `README.md` — walkthrough + drift fixes.
- `docs/specs/FEDERATION_PROTOCOL.md`, `docs/specs/HEALING_PROTOCOL.md`,
  `docs/specs/DATA_MODEL.sql`, `docs/specs/ARCHITECTURE_DECISIONS.md`,
  `docs/THREAT_MODEL.md`, `docs/ROADMAP.md`,
  `docs/superpowers/plans/phase2/2026-06-11-phase2-federation.md` — amendments.

---

### Task 1: Migration `0016_node_draining.sql` + manifest + behavior test

**Files:**
- Create: `internal/db/migrations/0016_node_draining.sql`
- Create: `internal/db/migrations/draining_state_test.go`
- Modify: `internal/db/migrations/MANIFEST.sha256` (append only)

**Interfaces:**
- Produces: `nodes.draining_at timestamptz` (NULL = not draining), partial index
  `nodes_draining_idx`. Every later task assumes this column exists.

- [ ] **Step 1: Write the failing migration behavior test**

Mirror the structure of the existing `internal/db/migrations/possession_state_test.go`
(same package, same `dbtest` harness — read it first and copy its setup shape).

```go
package migrations_test

// draining_state_test.go — P2-M7 migration 0016 behavior (D-M7-6a):
// draining_at exists, defaults NULL, is NOT touched by RegisterNode's
// ON CONFLICT re-register, and the partial index exists.

func TestDrainingColumnAndUpgrade(t *testing.T) {
	pool := dbtest.MustPool(t) // runs ALL migrations 0001..0016 forward

	// Column exists, defaults NULL.
	var draining *time.Time
	seedMinimalNode(t, pool, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT draining_at FROM nodes WHERE id = $1`,
		"11111111-1111-1111-1111-111111111111").Scan(&draining))
	require.Nil(t, draining, "fresh node must not be draining")

	// Partial index exists.
	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_indexes
		 WHERE tablename='nodes' AND indexname='nodes_draining_idx'`).Scan(&n))
	require.Equal(t, 1, n)

	// Re-register (the RegisterNode upsert) must NOT clear draining_at.
	_, err := pool.Exec(t.Context(),
		`UPDATE nodes SET draining_at = now() WHERE id = $1`,
		"11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	reRegisterNode(t, pool, "11111111-1111-1111-1111-111111111111") // calls gen.RegisterNode
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT draining_at FROM nodes WHERE id = $1`,
		"11111111-1111-1111-1111-111111111111").Scan(&draining))
	require.NotNil(t, draining, "re-register must not clear draining_at (D-M7-6d)")
}
```

`seedMinimalNode` / `reRegisterNode`: small helpers in this file — copy the node
INSERT shape and the `gen.RegisterNode` call from the existing migration/federation
tests (`internal/federation/coordinator/register_test.go` shows valid
`RegisterNodeParams`).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/db/migrations/ -run TestDrainingColumnAndUpgrade -v`
Expected: FAIL — `column "draining_at" does not exist`.

- [ ] **Step 3: Write the migration**

`internal/db/migrations/0016_node_draining.sql`:

```sql
-- +goose Up
-- +goose StatementBegin
-- P2-M7 (D-M7-6a): voluntary graceful drain. draining_at is the AUTHORITATIVE
-- drain marker (NULL = not draining). It is distinct from placement_weight = 0
-- (a placement throttle that still counts toward durability). Safety counts
-- (healthy/sourceable/prune/commit) exclude draining nodes; read/repair source
-- SELECTION may still use them, deprioritized. Set/cleared ONLY by
-- novactl node drain/undrain — never by register/heartbeat.
ALTER TABLE nodes ADD COLUMN draining_at timestamptz;

-- Partial index: drain-debt queries and the metrics scrape enumerate draining
-- nodes; the population is tiny, keep the index tiny.
CREATE INDEX nodes_draining_idx ON nodes (draining_at) WHERE draining_at IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX nodes_draining_idx;
ALTER TABLE nodes DROP COLUMN draining_at;
-- +goose StatementEnd
```

Append to the manifest (exactly the documented mechanism):

```bash
(cd internal/db/migrations && sha256sum 0016_node_draining.sql >> MANIFEST.sha256)
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/db/migrations/ -run TestDrainingColumnAndUpgrade -v && make migrations-frozen`
Expected: PASS, and `migrations-frozen` green.

- [ ] **Step 5: Commit**

```bash
git add internal/db/migrations/0016_node_draining.sql \
        internal/db/migrations/MANIFEST.sha256 \
        internal/db/migrations/draining_state_test.go
git commit -m "feat(p2-m7): migration 0016 nodes.draining_at + partial index; re-register preserves drain (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Query classification — safety counts vs placement vs selection + drain-debt queries

**Files:**
- Modify: `internal/db/queries/replication.sql`, `internal/db/queries/storage_state.sql`
- Create: `internal/db/queries/drain.sql`
- Regenerate: `internal/db/gen/` (`make sqlc-generate`)
- Test: `internal/orchestrator/drain_test.go` (query-semantics half)

**Interfaces:**
- Produces (sqlc, `internal/db/gen`):
  `SetNodeDraining(ctx, id pgtype.UUID) (int64, error)`,
  `ClearNodeDraining(ctx, id pgtype.UUID) (int64, error)`,
  `GetNodeDrainState(ctx, id) (GetNodeDrainStateRow{Status, AssignmentSyncState, DrainingAt}, error)`,
  `ListDrainingNodes(ctx) ([]ListDrainingNodesRow{ID, DrainingAt}, error)`,
  `CountDrainPendingCIDs(ctx, nodeID) (int64, error)`,
  `CountDrainInflightCIDs(ctx, nodeID) (int64, error)`,
  `CountBelowFloorReplicas(ctx, floor float64) ([]CountBelowFloorReplicasRow{NodeID, AckedReplicas}, error)`.
- Consumes: Task 1's column.

- [ ] **Step 1: Write the failing query-semantics tests**

In `internal/orchestrator/drain_test.go` (this package already has DB-backed test
plumbing — mirror `projection_test.go`'s `seedNode`/fixture helpers; if a helper is
unexported in another `_test.go` file of the same package, it is directly usable):

```go
// Drain query semantics (D-M7-6b/6c). One fixture: CID "drain-cid" with
// target_count=2, acked on nodes A (draining) and B (healthy); node C is an
// eligible empty destination.

func TestDrainExcludedFromSafetyCounts(t *testing.T) {
	// After marking A draining: RecomputeReplicationCounts(drain-cid)
	// healthy_acked == 1 (B only), sourceable_acked == 1 (B only).
}

func TestDrainExcludedFromPlacement(t *testing.T) {
	// ListPlacementCandidates(other-cid) with A draining: A absent, C present.
}

func TestDrainDeprioritizedNotExcludedFromSelection(t *testing.T) {
	// ListSourceableHolders(drain-cid): returns BOTH A and B, B FIRST even if
	// A.reputation_score > B.reputation_score (the prepended drain sort key wins).
	// ListRepairSourceHolders(drain-cid): with only A holding acked → returns A
	// (draining node remains a repair source of last resort).
}

func TestWeightZeroIsNotDrain(t *testing.T) {
	// B gets placement_weight=0, draining_at NULL:
	//   RecomputeReplicationCounts still counts B (healthy_acked includes it).
	// A gets placement_weight=1.0, draining_at=now():
	//   RecomputeReplicationCounts excludes A.
	// (Global constraint: the two markers are distinct in BOTH directions.)
}

func TestDrainDebtCounts(t *testing.T) {
	// target_count=2, holders: A(draining, acked) + B(healthy, acked)
	//   → CountDrainPendingCIDs(A) == 1 (only 1 non-draining acked holder < 2).
	// Add C acked → == 0.
	// Reset; give C only a PENDING assignment → still == 1 (pending is not safe)
	//   and CountDrainInflightCIDs(A) == 1.
}

func TestBelowFloorDebt(t *testing.T) {
	// B.reputation_score = 0.2 (< floor 0.5), holding 3 acked pins
	//   → CountBelowFloorReplicas(0.5) has row {B, 3}.
}
```

Write these as real tests against the fixture (full assertions, not comments — the
comments above pin the exact expected numbers).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/orchestrator/ -run 'TestDrain|TestWeightZero|TestBelowFloor' -v`
Expected: FAIL — `CountDrainPendingCIDs` undefined / counts wrong.

- [ ] **Step 3: Edit the safety-count and placement queries (NOT selection)**

`internal/db/queries/replication.sql` — `RecomputeReplicationCounts`: add a
`draining` column to the `holders` CTE and exclude it from `healthy_acked` AND
`sourceable_acked` (NOT from `in_flight`: drain fails the node's pendings in the
same transaction, so a pending-on-draining row is a transient race that the next
recompute resolves):

```sql
WITH holders AS (
    SELECT pa.state,
           n.status,
           n.assignment_sync_state,
           n.trust_state,
           (n.draining_at IS NOT NULL) AS draining,   -- P2-M7 D-M7-6c
           (n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> ''
            AND n.advertised_capabilities @> ARRAY['read-source/v1']) AS read_srcable
    FROM pin_assignments pa
    JOIN nodes n ON n.id = pa.node_id
    WHERE pa.cid = $1
)
SELECT
    count(*) FILTER (
        WHERE state = 'acked' AND status IN ('active', 'suspect')
          AND assignment_sync_state = 'current'
          AND NOT draining
    )::int AS healthy_acked,
    count(*) FILTER (
        WHERE state = 'acked' AND status IN ('active', 'suspect')
          AND assignment_sync_state = 'current'
          AND trust_state <> 'suspended' AND read_srcable
          AND NOT draining
    )::int AS sourceable_acked,
    ...            -- in_flight filter UNCHANGED — keep the rest of the query verbatim
```

`ListPlacementCandidates`: add one predicate line:

```sql
WHERE n.status = 'active'
  AND n.assignment_sync_state = 'current'
  AND n.trust_state <> 'suspended'
  AND n.draining_at IS NULL          -- P2-M7 D-M7-6c: never a new-placement destination
  AND NOT EXISTS (...)
```

`internal/db/queries/storage_state.sql` — `CountSourceableHolders` (commit/prune/
read-safety count): add `AND n.draining_at IS NULL` to its WHERE clause.

- [ ] **Step 4: Edit the selection queries — prepend the drain sort key ONLY**

`ListSourceableHolders` (draining stays *eligible*, sorted last):

```sql
ORDER BY (n.draining_at IS NOT NULL), n.reputation_score DESC, n.id;
```

`ListRepairSourceHolders`:

```sql
ORDER BY (n.draining_at IS NOT NULL),
         (COALESCE(n.last_egress_remaining_bytes, 0)::float8 * n.reputation_score) DESC,
         n.reputation_score DESC, n.id
LIMIT 1;
```

**Do NOT edit** `IsRepairSourceableForCID` / `GetRepairSource` — reservation-time
guards; a draining node must remain repair-sourceable (D-M7-6c).

- [ ] **Step 5: Add `internal/db/queries/drain.sql`**

```sql
-- P2-M7 (D-M7-6): voluntary graceful drain — marker + debt queries. Drain debt
-- deliberately queries pin_assignments ⨝ nodes directly; it does NOT overload
-- blob_replication_state.sourceable_acked_count (that is a safety count).

-- name: SetNodeDraining :execrows
-- One-shot; a second drain is a no-op here (idempotency: timestamp preserved).
UPDATE nodes SET draining_at = now()
WHERE id = $1 AND draining_at IS NULL;

-- name: ClearNodeDraining :execrows
UPDATE nodes SET draining_at = NULL
WHERE id = $1 AND draining_at IS NOT NULL;

-- name: GetNodeDrainState :one
SELECT status, assignment_sync_state, draining_at FROM nodes WHERE id = $1;

-- name: ListDrainingNodes :many
SELECT id, draining_at FROM nodes WHERE draining_at IS NOT NULL ORDER BY id;

-- name: CountDrainPendingCIDs :one
-- Drain debt (D-M7-6f): CIDs acked on the draining node whose count of acked,
-- live, sync-current, NON-draining holders is below target_count. Pending
-- reservations are NOT safe and do not reduce debt.
SELECT count(*) FROM (
    SELECT pa.cid
    FROM pin_assignments pa
    JOIN blob_replication_state brs ON brs.cid = pa.cid
    WHERE pa.node_id = $1 AND pa.state = 'acked'
      AND (SELECT count(*)
           FROM pin_assignments pa2
           JOIN nodes n2 ON n2.id = pa2.node_id
           WHERE pa2.cid = pa.cid AND pa2.state = 'acked'
             AND n2.status IN ('active','suspect')
             AND n2.assignment_sync_state = 'current'
             AND n2.draining_at IS NULL) < brs.target_count
) debt;

-- name: CountDrainInflightCIDs :one
-- Replacement in progress but not acked: lets an operator distinguish "stuck"
-- from "working" (D-M7-6f).
SELECT count(DISTINCT pa.cid)
FROM pin_assignments pa
WHERE pa.node_id = $1 AND pa.state = 'acked'
  AND EXISTS (SELECT 1 FROM pin_assignments p3
              WHERE p3.cid = pa.cid AND p3.state = 'pending');

-- name: CountBelowFloorReplicas :many
-- Below-floor replica debt (D-M7-1a): acked replicas on live nodes below the
-- reputation floor whose pins have not hard-failed. Observability only — the
-- automated remedy is P2-M6.1, NOT this milestone.
SELECT n.id AS node_id, count(*) AS acked_replicas
FROM pin_assignments pa
JOIN nodes n ON n.id = pa.node_id
WHERE pa.state = 'acked'
  AND n.status IN ('active','suspect')
  AND n.reputation_score < sqlc.arg(floor)::float8
GROUP BY n.id
ORDER BY n.id;
```

Run `make sqlc-generate`.

- [ ] **Step 6: Run to verify tests pass**

Run: `go test ./internal/orchestrator/ -run 'TestDrain|TestWeightZero|TestBelowFloor' -v && make codegen-check && go build ./...`
Expected: PASS, no codegen drift, whole tree builds.

Also run the neighbors that consume the edited queries:
`go test ./internal/orchestrator/ ./pkg/coordinator/... ./internal/federation/coordinator/ -count=1`
Expected: PASS (no existing test seeds `draining_at`, so semantics are unchanged for them).

- [ ] **Step 7: Commit**

```bash
git add internal/db/queries/replication.sql internal/db/queries/storage_state.sql \
        internal/db/queries/drain.sql internal/db/gen internal/orchestrator/drain_test.go
git commit -m "feat(p2-m7): drain query classification — safety counts exclude, selection deprioritizes; drain/below-floor debt queries (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: `novactl node drain` / `undrain`

**Files:**
- Create: `cmd/novactl/node_drain.go`, `cmd/novactl/node_drain_test.go`
- Modify: `cmd/novactl/node.go` (switch + usage string)

**Interfaces:**
- Consumes: Task 2's gen queries.
- Produces: testable cores
  `drainNode(ctx, pool *pgxpool.Pool, id pgtype.UUID, force bool) (drainResult, error)`
  and `undrainNode(ctx, pool *pgxpool.Pool, id pgtype.UUID) error`;
  `drainResult{AlreadyDraining bool; PendingCIDs, InflightCIDs int64}`.
  CLI: `novactl node drain --id <uuid> [--force] [--no-confirm]`,
  `novactl node undrain --id <uuid>`.

- [ ] **Step 1: Write the failing core tests**

`cmd/novactl/node_drain_test.go`, mirroring `node_db_test.go`'s `TestRevokeNodeCore`
DB harness:

```go
func TestDrainNodeCore(t *testing.T) {
	// Fixture: node A active/current with 1 acked + 1 pending assignment.
	// drainNode(A):
	//   - nodes.draining_at IS NOT NULL
	//   - A's pending assignment is now 'failed' (FailNodePendingAssignments ran)
	//   - blob_replication_reconcile_queue has A's CIDs with reason='node_draining'
	//   - result.AlreadyDraining == false
}

func TestDrainNodeIdempotent(t *testing.T) {
	// Second drainNode(A): AlreadyDraining == true, draining_at UNCHANGED
	// (compare timestamps), CIDs re-enqueued without error.
}

func TestDrainNodeRefusals(t *testing.T) {
	// status='revoked'                       → error mentioning "not active/suspect"
	// assignment_sync_state='reconciling'    → error mentioning "--force"
	// same node with force=true              → drains
}

func TestUndrainNodeCore(t *testing.T) {
	// undrainNode(A) after drain: draining_at IS NULL, queue has A's CIDs
	// with reason='node_undrained'.
	// undrain on a non-draining node → error "not draining".
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cmd/novactl/ -run 'TestDrainNode|TestUndrainNode' -v`
Expected: FAIL — `drainNode` undefined.

- [ ] **Step 3: Implement `cmd/novactl/node_drain.go`**

```go
package main

// P2-M7 (D-M7-6): voluntary graceful drain — the safe VOLUNTARY decommission
// primitive. Node-scoped, operator-initiated, one-shot, non-hysteretic; it is
// NOT the P2-M6.1 below-floor replacement queue. revoke stays the involuntary
// path. Steps 3–5 of the design (mark, fail pendings, enqueue) run in ONE
// transaction — the same bulk-transition contract as the liveness sweeper.

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
)

type drainResult struct {
	AlreadyDraining bool
	PendingCIDs     int64
	InflightCIDs    int64
}

// drainNode is the testable core.
func drainNode(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID, force bool) (drainResult, error) {
	var res drainResult
	q := gen.New(pool)
	st, err := q.GetNodeDrainState(ctx, id)
	if err != nil {
		return res, fmt.Errorf("node not found: %w", err)
	}
	if st.Status != gen.NodeStatusActive && st.Status != gen.NodeStatusSuspect {
		return res, fmt.Errorf("node is %s, not active/suspect — drain is for live nodes; use revoke for %s nodes", st.Status, st.Status)
	}
	if st.AssignmentSyncState != "current" && !force {
		return res, fmt.Errorf("node sync state is %q (unstable CID set); re-run with --force to drain anyway", st.AssignmentSyncState)
	}
	res.AlreadyDraining = st.DrainingAt.Valid

	tx, err := pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	qtx := gen.New(tx)
	if !res.AlreadyDraining {
		if _, err := qtx.SetNodeDraining(ctx, id); err != nil {
			return res, err
		}
	}
	if _, err := qtx.FailNodePendingAssignments(ctx, id); err != nil {
		return res, err
	}
	if err := qtx.MarkReplicationDirtyForNode(ctx, id); err != nil {
		return res, err
	}
	if err := qtx.EnqueueReconcileForNode(ctx, gen.EnqueueReconcileForNodeParams{
		Reason: "node_draining", NodeID: id,
	}); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}

	res.PendingCIDs, _ = q.CountDrainPendingCIDs(ctx, id)
	res.InflightCIDs, _ = q.CountDrainInflightCIDs(ctx, id)
	return res, nil
}

// undrainNode is the tiny explicit inverse (D-M7-6e).
func undrainNode(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := gen.New(tx)
	n, err := qtx.ClearNodeDraining(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("node not found or not draining")
	}
	if err := qtx.MarkReplicationDirtyForNode(ctx, id); err != nil {
		return err
	}
	if err := qtx.EnqueueReconcileForNode(ctx, gen.EnqueueReconcileForNodeParams{
		Reason: "node_undrained", NodeID: id,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func cmdNodeDrain(args []string) error {
	fs := flag.NewFlagSet("node drain", flag.ContinueOnError)
	idStr := fs.String("id", "", "node id (uuid)")
	force := fs.Bool("force", false, "drain even if assignment_sync_state != current")
	noConfirm := fs.Bool("no-confirm", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pgID, err := parsePGUUID(*idStr)
	if err != nil {
		return err
	}
	if !*noConfirm {
		fmt.Printf("Drain node %s? It stops receiving placements and stops counting toward durability, but keeps serving as a source while its replicas are rebuilt. [y/N]: ", *idStr)
		var ans string
		fmt.Scanln(&ans)
		if ans != "y" && ans != "Y" {
			return errors.New("aborted")
		}
	}
	return withNodeDBPool(func(ctx context.Context, pool *pgxpool.Pool) error {
		res, err := drainNode(ctx, pool, pgID, *force)
		if err != nil {
			return err
		}
		if res.AlreadyDraining {
			fmt.Printf("node %s was already draining — CIDs re-enqueued\n", *idStr)
		} else {
			fmt.Printf("draining node %s\n", *idStr)
		}
		fmt.Printf("drain debt: %d CIDs below target (%d with replacement in flight)\n", res.PendingCIDs, res.InflightCIDs)
		fmt.Println("next: keep the donor RUNNING; watch nova_node_drain_pending_cids; `novactl node revoke` only once debt is 0")
		return nil
	})
}

func cmdNodeUndrain(args []string) error {
	fs := flag.NewFlagSet("node undrain", flag.ContinueOnError)
	idStr := fs.String("id", "", "node id (uuid)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pgID, err := parsePGUUID(*idStr)
	if err != nil {
		return err
	}
	return withNodeDBPool(func(ctx context.Context, pool *pgxpool.Pool) error {
		if err := undrainNode(ctx, pool, pgID); err != nil {
			return err
		}
		fmt.Printf("node %s is no longer draining; countability/placement return on the next recompute\n", *idStr)
		return nil
	})
}
```

`cmd/novactl/node.go`: add to the switch and to the usage string:

```go
	case "drain":
		return cmdNodeDrain(args[1:])
	case "undrain":
		return cmdNodeUndrain(args[1:])
```

(usage: `<ca-init|issue|issue-coordinator-client|revoke|rotate-cert|list|set-domain|nebula-template|trust|drain|undrain>`)

- [ ] **Step 4: Run to verify tests pass**

Run: `go test ./cmd/novactl/ -run 'TestDrainNode|TestUndrainNode' -v && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/novactl/node_drain.go cmd/novactl/node_drain_test.go cmd/novactl/node.go
git commit -m "feat(p2-m7): novactl node drain/undrain — one-shot, transactional, two-step decommission UX (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: `metrics_listen_addr` config + `internal/metrics` package + coordinator wiring

**Files:**
- Create: `internal/metrics/metrics.go`, `internal/metrics/collector.go`,
  `internal/metrics/server.go`, `internal/metrics/metrics_test.go`
- Modify: `internal/config/types.go`, `internal/config/operator_yaml.go`,
  `cmd/coordinator/main.go`, `go.mod` (client_golang → direct via `go mod tidy`)

**Interfaces:**
- Produces:
  `config.Config.EffectiveMetricsListenAddr() (addr string, enabled bool)`;
  `metrics.New(pool *pgxpool.Pool, reputationFloor float64) *Metrics`;
  `(*Metrics).Handler() http.Handler`;
  `(*Metrics).Registry() *prometheus.Registry` (for tests);
  hook methods (Task 5 wires them):
  `ObserveRegisterFailure(reason string)`,
  `ObserveTrustTransition(from, to, reason string)`,
  `ObserveReputationMove(direction string)`,
  `ObserveAuditLatency(seconds float64)`,
  `ObserveDonorFetch(result, reason string, seconds float64)`,
  `ObserveEgressRefusal(reason string)`,
  `ObserveSourceSelectionFailure(reason string)`;
  `metrics.ListenAndServe(ctx, addr string, h http.Handler) error` (fatal bind).

- [ ] **Step 1: Write the failing tests**

`internal/metrics/metrics_test.go` (uses `internal/dbtest`):

```go
func TestConfigMetricsListenAddr(t *testing.T) {
	// (in internal/config tests if more natural — either location fine)
	// absent key            → ("127.0.0.1:2112", true)
	// metrics_listen_addr: ""      → ("", false)   — explicit disable
	// metrics_listen_addr: "127.0.0.1:9999" → ("127.0.0.1:9999", true)
}

func TestScrapeFamiliesAndValues(t *testing.T) {
	// Seed: 1 node active/current draining with 1 acked CID (target 2, one
	// healthy holder besides it? no — alone), 1 node below floor with 2 acked.
	// GET /metrics via httptest against m.Handler():
	//   nova_nodes{status="active",...}          >= 1
	//   nova_node_draining                        == 1
	//   nova_node_drain_pending_cids{node_id=...} == 1
	//   nova_below_floor_replica_debt             == 2
	//   nova_replication_cids{...}                present
	//   nova_reconcile_queue_depth{...}           present
}

func TestLabelDiscipline(t *testing.T) {
	// Gather() every family from m.Registry(); assert NO label name is in
	// {"cid","blob","path","filename","url","collection"} (Global Constraint).
	// Also touch every process-local hook once first so their families exist:
	// m.ObserveRegisterFailure("missing_capability"); m.ObserveTrustTransition(...); etc.
}

func TestBindFailureIsFatal(t *testing.T) {
	// net.Listen on a port, then metrics.ListenAndServe(ctx, sameAddr, h)
	// must return a non-nil error immediately (startup-fatal contract).
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/metrics/ ./internal/config/ -run 'TestConfigMetrics|TestScrape|TestLabel|TestBind' -v`
Expected: FAIL — package/fields undefined.

- [ ] **Step 3: Implement config**

`internal/config/types.go` — in the `Coordinator` struct (tri-state pointer, the
`RecordSourceIP` precedent):

```go
	// MetricsListenAddr (P2-M7, D-M7-1) binds the coordinator-only Prometheus
	// /metrics listener — its own plane, never the public/admin/federation mux.
	// Tri-state: nil = default 127.0.0.1:2112 (enabled, loopback); explicit ""
	// = disabled (a deliberate operator act); any other value = that address.
	// A bind failure while enabled is startup-fatal.
	MetricsListenAddr *string `yaml:"metrics_listen_addr,omitempty"`
```

Accessor (same file, near the other `Effective*` helpers):

```go
// EffectiveMetricsListenAddr resolves the D-M7-1 tri-state.
func (c *Config) EffectiveMetricsListenAddr() (string, bool) {
	v := c.Coordinator.MetricsListenAddr
	if v == nil {
		return "127.0.0.1:2112", true
	}
	if *v == "" {
		return "", false
	}
	return *v, true
}
```

`operator_yaml.go` `validate`: refuse a non-empty value that does not parse:

```go
	if addr, enabled := cfg.EffectiveMetricsListenAddr(); enabled {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("config: metrics_listen_addr %q is not host:port: %w", addr, err)
		}
	}
```

- [ ] **Step 4: Implement `internal/metrics`**

`metrics.go` — registry + process-local instruments + hooks:

```go
// Package metrics is the P2-M7 (D-M7-1) coordinator-only observability surface.
// Two source kinds (D-M7-1a): DB-derived gauges/counters computed at scrape time
// from durable projections (collector.go), and process-local counters/histograms
// instrumented at event sites via the Observe* hooks — those reset on restart,
// which is normal Prometheus counter behavior; do NOT "fix" a reset by inventing
// a durable event table. Label discipline: bounded label sets only; NEVER
// per-CID/blob/path/filename labels (enforced by TestLabelDiscipline).
package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

type Metrics struct {
	reg *prometheus.Registry

	registerFailures  *prometheus.CounterVec   // reason
	trustTransitions  *prometheus.CounterVec   // from,to,reason
	reputationMoved   *prometheus.CounterVec   // direction
	auditLatency      prometheus.Histogram
	donorFetch        *prometheus.CounterVec   // result,reason
	donorFetchLatency prometheus.Histogram
	egressRefusals    *prometheus.CounterVec   // reason
	sourceSelFailures *prometheus.CounterVec   // reason
}

func New(pool *pgxpool.Pool, reputationFloor float64) *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}
	m.registerFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_compat_registration_failures_total",
		Help: "fed/v1 register rejections by reason (process-local; resets on restart).",
	}, []string{"reason"})
	m.trustTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_trust_transitions_total",
		Help: "Trust state transitions (process-local).",
	}, []string{"from", "to", "reason"})
	m.reputationMoved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_reputation_moved_total",
		Help: "Reputation movements by direction (process-local).",
	}, []string{"direction"})
	m.auditLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "nova_audit_latency_seconds", Help: "Possession-audit round-trip latency.",
		Buckets: prometheus.DefBuckets,
	})
	m.donorFetch = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_donor_fetch_total", Help: "Donor-backed read fetches by outcome (process-local).",
	}, []string{"result", "reason"})
	m.donorFetchLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "nova_donor_fetch_latency_seconds", Help: "Donor-backed read fetch latency.",
		Buckets: prometheus.DefBuckets,
	})
	m.egressRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_donor_egress_refusals_total", Help: "Donor egress-budget refusals observed by the coordinator.",
	}, []string{"reason"})
	m.sourceSelFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_source_selection_failures_total", Help: "Read/repair source selection failures by reason.",
	}, []string{"reason"})
	m.reg.MustRegister(m.registerFailures, m.trustTransitions, m.reputationMoved,
		m.auditLatency, m.donorFetch, m.donorFetchLatency, m.egressRefusals,
		m.sourceSelFailures,
		newDBCollector(pool, reputationFloor)) // collector.go
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Hook methods — the ONLY coupling event sites have to this package is a
// func value / tiny interface (Task 5); they never import prometheus.
func (m *Metrics) ObserveRegisterFailure(reason string) { m.registerFailures.WithLabelValues(reason).Inc() }
func (m *Metrics) ObserveTrustTransition(from, to, reason string) {
	m.trustTransitions.WithLabelValues(from, to, reason).Inc()
}
func (m *Metrics) ObserveReputationMove(direction string) { m.reputationMoved.WithLabelValues(direction).Inc() }
func (m *Metrics) ObserveAuditLatency(sec float64)        { m.auditLatency.Observe(sec) }
func (m *Metrics) ObserveDonorFetch(result, reason string, sec float64) {
	m.donorFetch.WithLabelValues(result, reason).Inc()
	m.donorFetchLatency.Observe(sec)
}
func (m *Metrics) ObserveEgressRefusal(reason string)          { m.egressRefusals.WithLabelValues(reason).Inc() }
func (m *Metrics) ObserveSourceSelectionFailure(reason string) { m.sourceSelFailures.WithLabelValues(reason).Inc() }
```

`collector.go` — DB-derived families at scrape time (implements
`prometheus.Collector`; 3-second scrape context; on query error, log at warn and
emit nothing — a broken scrape must not panic the handler):

```go
// newDBCollector reads durable projections at scrape time (D-M7-1a):
//   nova_replication_cids{tier,class}            blob_replication_state
//   nova_reconcile_queue_depth{reason}           blob_replication_reconcile_queue
//   nova_reconcile_queue_oldest_seconds{reason}  ...
//   nova_nodes{status,trust_state,assignment_sync_state}
//   nova_below_floor_replica_debt / nova_node_below_floor_replicas{node_id}   CountBelowFloorReplicas
//   nova_node_draining / _drain_pending_cids / _drain_inflight_cids /
//   _drain_pending_oldest_seconds / _drain_ready {node_id}                    ListDrainingNodes + Count* per node
//   nova_audit_results_total{result,reason}      durable pin_audits rows (restart-stable counter)
```

Implement `Describe`/`Collect` with `prometheus.MustNewConstMetric`. The tier/queue/
nodes families are one `GROUP BY` SQL each (inline via `pool.Query`, matching the
grouping the projection tables already index). Drain families iterate
`ListDrainingNodes` → `CountDrainPendingCIDs` / `CountDrainInflightCIDs` per node
(bounded: the draining population is operator-initiated and tiny);
`_drain_pending_oldest_seconds` = `now − draining_at` while debt > 0 else 0;
`_drain_ready` = 1 when debt == 0.

`server.go`:

```go
// ListenAndServe binds addr FIRST (so a bind failure is a returned error the
// caller treats as startup-fatal, D-M7-1) and serves until ctx is done.
func ListenAndServe(ctx context.Context, addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics: bind %s: %w", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	slog.Info("metrics.listening", "addr", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
```

`cmd/coordinator/main.go` wiring (next to the orchestrator/possession block; the
pool and cfg are in scope there):

```go
	// P2-M7 (D-M7-1): coordinator-only Prometheus plane. Bind failure is fatal.
	var mtr *metrics.Metrics
	if addr, enabled := cfg.EffectiveMetricsListenAddr(); enabled {
		mtr = metrics.New(pool, cfg.Orchestrator.Replication.ReputationFloor)
		mln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("metrics_listen_addr: %w", err)
		}
		g.Go(func() error { return metrics.ServeListener(ctx, mln, mtr.Handler()) })
	}
```

> **Implementation note (caution 1):** match `main.go`'s actual error/goroutine
> idiom — if it uses `errc <- run(ctx)` rather than an errgroup, expose
> `metrics.ServeListener(ctx, ln, h)` (same as `ListenAndServe` but taking the
> pre-bound listener) and start it the way the federation listener is started.
> The invariant to preserve is only: **bind before any goroutine; error out of
> startup on bind failure.**

- [ ] **Step 5: Run to verify tests pass**

Run: `go mod tidy && go test ./internal/metrics/ ./internal/config/ -v -count=1 && go build ./...`
Expected: PASS; `go.mod` now lists `prometheus/client_golang` as direct.

- [ ] **Step 6: Commit**

```bash
git add internal/metrics internal/config/types.go internal/config/operator_yaml.go \
        cmd/coordinator/main.go go.mod go.sum
git commit -m "feat(p2-m7): coordinator-only Prometheus /metrics — dedicated loopback listener, DB-derived + process-local families, fatal bind (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: Process-local instrumentation hooks at the event sites

**Files:**
- Modify: `internal/federation/coordinator/server.go` (+`handlers.go`),
  `internal/audit/possession/verify.go` (+`trust.go`),
  `pkg/coordinator/storage/readsource.go`, `cmd/coordinator/main.go`
- Test: extend the packages' existing `_test.go` files

**Interfaces:**
- Consumes: `*metrics.Metrics` hook methods (Task 4).
- Produces: nil-safe hook fields — `ServerConfig.OnRegisterFailure func(reason
  string)`; `possession.Observer` interface `{ TrustTransition(from, to, reason
  string); ReputationMoved(direction string); AuditLatency(seconds float64) }` +
  `(*Auditor).SetObserver(Observer)`; readsource observer
  `{ Fetch(result, reason string, seconds float64); EgressRefusal(reason string);
  SelectionFailure(reason string) }` + `(*Service).SetReadObserver(...)`.

- [ ] **Step 1: Write the failing hook tests** — one per package, using a recording
  fake (e.g. in `internal/federation/coordinator/compat_matrix_test.go` or the
  existing `register_test.go`):

```go
func TestRegisterFailureHookFires(t *testing.T) {
	// ServerConfig.OnRegisterFailure = record; register a donor missing a
	// required capability → recorded reason == "missing_capability";
	// register with wrong protocol → "incompatible_protocol".
	// nil hook (default) must not panic.
}
// possession: drive Auditor.Record through a pass + a graduation fixture
// (the existing trust tests have the fixture) → observer saw
// TrustTransition("probationary","trusted","graduated") and AuditLatency > 0.
// readsource: reuse setDonorReadSourceForTest seams; a failed fetch records
// Fetch("error", <reason>, _); an exhausted-budget donor records EgressRefusal.
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/federation/coordinator/ ./internal/audit/possession/ ./pkg/coordinator/storage/ -run 'Hook|Observer' -v`
Expected: FAIL — fields undefined.

- [ ] **Step 3: Implement** — each hook is a nil-guarded one-liner at the existing
  decision point (grep anchors):

  - `handlers.go`: at the two `writeError(..., "incompatible_protocol", ...)` /
    `"missing_capability"` returns → `if s.cfg.OnRegisterFailure != nil {
    s.cfg.OnRegisterFailure("incompatible_protocol") }` (resp. `missing_capability`).
  - `possession/trust.go` `applyTrust`: where graduation/demotion commit → observer
    `TrustTransition(...)`; `verify.go` `Record`: reputation write → `ReputationMoved`,
    outcome timing → `AuditLatency`.
  - `readsource.go` `attemptHolder`/`selectAndFetch`: outcome → `Fetch(...)`;
    donor 429/budget refusal branch → `EgressRefusal`; "no sourceable holder" →
    `SelectionFailure("no_sourceable_holder")`.
  - `main.go`: after Task 4's `mtr` is built, wire:
    `fedCfg.OnRegisterFailure = mtr.ObserveRegisterFailure`;
    `auditor.SetObserver(mtr)`; `storageSvc.SetReadObserver(mtr)` — via a tiny
    adapter struct if the two interfaces' method names differ from `Metrics`'s
    (define the adapter next to the wiring; keep the consuming packages
    prometheus-free).

- [ ] **Step 4: Run to verify green + no donor pollution**

Run: `go test ./internal/federation/coordinator/ ./internal/audit/possession/ ./pkg/coordinator/storage/ -count=1 && ./scripts/check_node_deps.sh`
Expected: PASS; boundary clean (hooks are func values/interfaces — no prometheus
import outside `internal/metrics` + `cmd/coordinator`).

- [ ] **Step 5: Commit**

```bash
git add internal/federation/coordinator internal/audit/possession pkg/coordinator/storage cmd/coordinator/main.go
git commit -m "feat(p2-m7): nil-safe observability hooks at register/trust/audit/read-source event sites (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: Donor boundary — explicit `github.com/prometheus/*` deny (red → green)

**Files:**
- Modify: `scripts/check_node_deps.sh`

**Interfaces:** none (CI gate).

- [ ] **Step 1: Add the explicit deny.** The allowlist already rejects
  non-allowlisted packages; the explicit deny makes the prometheus prohibition
  immune to any future allowlist broadening (defense-in-depth, D-M7-1). After the
  `ALLOWED` array:

```bash
# P2-M7 (D-M7-1): metrics are coordinator-only. HARD DENY — even a future
# allowlist broadening must not admit a metrics stack into the donor graph.
DENIED_PREFIXES=("github.com/prometheus")
```

and inside the per-dep loop, before the allowlist check:

```bash
  for d in "${DENIED_PREFIXES[@]}"; do
    case "$p" in "$d"|"$d"/*)
      echo "FAIL: cmd/node imports HARD-DENIED package: $p (metrics are coordinator-only, D-M7-1)" >&2
      exit 1 ;;
    esac
  done
```

- [ ] **Step 2: Demonstrate RED.** Temporarily add to `cmd/node/main.go`:
  `import _ "github.com/prometheus/client_golang/prometheus"` — run
  `./scripts/check_node_deps.sh`. Expected: FAIL with the HARD-DENIED message.
  **Revert the import** (do not commit it).

- [ ] **Step 3: Verify GREEN.**

Run: `./scripts/check_node_deps.sh`
Expected: `OK: cmd/node dependency boundary clean`.

- [ ] **Step 4: Commit**

```bash
git add scripts/check_node_deps.sh
git commit -m "feat(p2-m7): hard-deny github.com/prometheus/* in the donor dependency boundary (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: Corpus-scale bench harness + Make targets + CI regression job

**Files:**
- Create: `internal/benchcorpus/seed.go`, `internal/benchcorpus/bench_test.go`,
  `internal/benchcorpus/explain_test.go`, `reports/benchmarks/.gitkeep`
- Modify: `Makefile`, `.github/workflows/ci.yml`

**Interfaces:**
- Produces: `make bench-corpus` (release gate, env `BENCH_ROWS=9800000
  BENCH_PROFILE=release`), `make bench-corpus-ci` (`BENCH_ROWS=250000
  BENCH_PROFILE=ci`), `make bench-corpus-explain`; artifacts
  `reports/benchmarks/p2-m7-corpus-<date>.{json,md}`.

- [ ] **Step 1: Write the DSN-guard + seeder tests first**

```go
func TestScratchDSNGuard(t *testing.T) {
	// resolveBenchPool with BENCH_DATABASE_URL="postgres://u@h/prod_db":
	//   → error containing "scratch" (refused).
	// same + BENCH_ALLOW_NON_SCRATCH=1 → no guard error (connection may still fail).
	// BENCH_DATABASE_URL unset → testcontainers pool (inherently scratch).
}

func TestSeedSkewedCorpus(t *testing.T) {
	// SeedCorpus(pool, 5_000 rows, 8 donors, seed=42):
	//   blob_blocks count == 5_000; >1 donor exists; the top donor holds ≥ 3×
	//   the bottom donor's assignments (zipf skew is real, not uniform).
}
```

- [ ] **Step 2: Run to verify FAIL** — `go test ./internal/benchcorpus/ -v`.

- [ ] **Step 3: Implement `seed.go`**

```go
// Package benchcorpus is the P2-M7 (D-M7-2) corpus-scale benchmark: a LOCAL
// milestone-exit gate (BENCH_PROFILE=release at ~9.8M blob_blocks) plus a small
// CI regression profile. SAFETY: scratch DB only — an external DSN is refused
// unless its database name contains "scratch" or BENCH_ALLOW_NON_SCRATCH=1.
// Artifacts are lightweight JSON/MD reports, never DB dumps.
package benchcorpus
```

`resolveBenchPool(tb)`: env DSN → guard (parse DSN, check `strings.Contains(dbname,
"scratch")` unless override) → `pgxpool.New`; else `dbtest.MustPool(tb)`.
`SeedCorpus(ctx, pool, rows int64, donors int, seed int64)`: batched
`generate_series` INSERTs — nodes (donors, mixed reputation/trust, one draining),
blobs + `blob_manifests` + `blob_blocks` (rows), `pin_assignments` with
**Zipf-distributed** donor choice (`math/rand/v2` `Zipf` with s=1.2 over donor
index), `blob_replication_state` rows (mixed tiers), a partially-filled
`blob_replication_reconcile_queue`. Keep each INSERT batch ≤ 10k values.

- [ ] **Step 4: Implement `bench_test.go`** — env-gated:

```go
func TestCorpusBench(t *testing.T) {
	rows := os.Getenv("BENCH_ROWS")
	if rows == "" { t.Skip("set BENCH_ROWS to run the corpus bench") }
	// seed → measure hot paths, N=200 iterations each over random fixture keys:
	//   RecomputeReplicationCounts, ListSourceableHolders, ListRepairSourceHolders,
	//   ListPlacementCandidates, ListReconcileBatch(+DeleteReconciled),
	//   SelectDueAuditNodes + SelectAckedPinForAudit + SelectRandomBlockForCID,
	//   CountDrainPendingCIDs, CountBelowFloorReplicas,
	//   DELETE cascade of one blob, full projection rebuild for 1k CIDs.
	// Record sorted durations → p50/p95/p99; EXPLAIN (ANALYZE, BUFFERS) once per path.
	// Thresholds: profiles["release"] (gate) vs profiles["ci"] (10× slack, shape only).
	// Write reports/benchmarks/p2-m7-corpus-<YYYY-MM-DD>.json + .md
	// (hardware via runtime.NumCPU + os hints, PG version via SELECT version(),
	//  Go version, row counts, seed, per-path percentiles, thresholds, pass/fail).
}
```

Thresholds live in one map (`profiles = map[string]map[string]time.Duration`);
release values are the gate (fill with the measured M6-era baselines ×2 headroom on
first release run — the FIRST full run records baselines, the artifact documents
them, and the committed map is updated in the same task).

- [ ] **Step 5: Implement `explain_test.go`** — deterministic, small fixture, no
  timing: for each hot query above, run `EXPLAIN (FORMAT JSON)` and assert the plan
  uses an index (reject `Seq Scan` on `pin_assignments`/`blob_blocks`/`pin_audits`
  for the keyed lookups; assert `nodes_draining_idx` serves `ListDrainingNodes`).

- [ ] **Step 6: Makefile + CI**

```make
.PHONY: bench-corpus bench-corpus-ci bench-corpus-explain
# P2-M7 D-M7-2: full-scale LOCAL release gate (hours; scratch DB only).
bench-corpus:
	BENCH_ROWS=9800000 BENCH_PROFILE=release go test -v -timeout 240m -run TestCorpusBench ./internal/benchcorpus
# CI regression profile: shape + plans, small corpus.
bench-corpus-ci:
	BENCH_ROWS=250000 BENCH_PROFILE=ci go test -v -timeout 30m -run TestCorpusBench ./internal/benchcorpus
bench-corpus-explain:
	go test -v -run 'TestExplainPlans|TestScratchDSNGuard|TestSeedSkewedCorpus' ./internal/benchcorpus
```

`.github/workflows/ci.yml` — new job (mirror the `test` job's Go setup; no libvips
needed):

```yaml
  bench-regression:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: Query-plan gate
        run: make bench-corpus-explain
      - name: Corpus regression bench (small profile)
        run: make bench-corpus-ci
        env:
          TESTCONTAINERS_RYUK_DISABLED: "true"
```

- [ ] **Step 7: Verify** — `make bench-corpus-explain` PASS;
  `BENCH_ROWS=20000 BENCH_PROFILE=ci go test -run TestCorpusBench ./internal/benchcorpus -v`
  PASS and writes an artifact.

- [ ] **Step 8: Commit**

```bash
git add internal/benchcorpus reports/benchmarks/.gitkeep Makefile .github/workflows/ci.yml
git commit -m "feat(p2-m7): corpus-scale bench harness — scratch-guarded, skewed seed, release/ci threshold split, EXPLAIN gate (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 8: Capability-matrix compatibility tests

**Files:**
- Create: `internal/federation/coordinator/compat_matrix_test.go`

**Interfaces:** consumes the existing register test harness
(`register_test.go` shows server construction + mTLS-free handler-level testing),
`wire.Cap*` constants, and the selection queries.

- [ ] **Step 1: Write the failing matrix tests**

```go
// D-M7-3: "required" is the CONFIGURED ServerConfig.RequiredCapabilities set.
// The canonical M7 required profile is {CapPinChangeLog, CapSnapshot}; the four
// route-gated capabilities must NOT appear in it (a capability cannot be both).

func TestRequiredCapabilityRejectsRegistration(t *testing.T) {
	// cfg.RequiredCapabilities = {pin-change-log/v1, snapshot/v1}
	// donor advertises only {pin-change-log/v1} → 400 missing_capability "snapshot/v1"
	// donor with no fed/v1 in SupportedProtocols → 400 incompatible_protocol
}

func TestRouteGatedCapabilitiesRegisterFine(t *testing.T) {
	// same required profile; donor advertises ONLY the two required caps
	// (none of blob-transfer/read-source/repair-stream/audit-block-hash)
	// → register 200. This PROVES the four are route-gated, not required.
}

func TestRouteGatedSelectionSkips(t *testing.T) {
	// Seed an acked holder n1 WITHOUT read-source/v1 (+addr set):
	//   ListSourceableHolders(cid) omits n1; CountSourceableHolders == 0.
	// n1 without repair-stream/v1: ListRepairSourceHolders(cid) → no row;
	//   IsRepairSourceableForCID(n1,cid) == false.
	// n1 without audit-block-hash/v1: SelectDueAuditNodes omits n1.
	// In each case a second holder n2 WITH the capability IS selected.
}

func TestCanonicalRequiredProfileDisjointFromRouteGated(t *testing.T) {
	// Guard: intersection(requiredProfile, routeGated) is empty — keeps the
	// spec's classification honest if someone edits the profile later.
}
```

- [ ] **Step 2: Run FAIL** — `go test ./internal/federation/coordinator/ -run TestRequired -run TestRouteGated -v` (file doesn't exist).

- [ ] **Step 3: Implement** — the two profile vars at the top of the test file:

```go
var requiredProfile = []string{wire.CapPinChangeLog, wire.CapSnapshot}
var routeGated = []string{wire.CapBlobTransfer, wire.CapReadSource, wire.CapRepairStream, wire.CapAuditBlockHash}
```

and fill the four tests against the existing harness (HTTP register calls +
gen-query assertions on a `dbtest` pool).

- [ ] **Step 4: Run PASS** — `go test ./internal/federation/coordinator/ -run 'TestRequired|TestRouteGated|TestCanonical' -v`

- [ ] **Step 5: Commit**

```bash
git add internal/federation/coordinator/compat_matrix_test.go
git commit -m "test(p2-m7): capability matrix — configured-required rejection vs route-gated graceful skip (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 9: Cross-version e2e script (`N−1 × HEAD` matrix)

**Files:**
- Create: `scripts/crossversion_e2e.sh`
- Modify: `Makefile` (`crossversion-e2e` target)

**Interfaces:** consumes the `p2-m6-possession-audits` tag; produces a local gate
(like `bench-corpus`, not a per-push CI job).

- [ ] **Step 1: Write the script skeleton with real mechanics**

```bash
#!/usr/bin/env bash
# P2-M7 (D-M7-3): mixed-version binary compatibility. Pairings:
#   head-head | head-coord-old-donor | old-coord-head-donor | all
# EVERY pairing proves join → serve → audit. Drain/decommission is exercised
# ONLY when the coordinator is HEAD (an N−1 coordinator has no drain).
# NOT a schema-downgrade test: each coordinator runs against the schema its OWN
# migrate binary produced (HEAD → 0016; N−1 → 0015). DB-upgrade coverage lives
# in internal/db/migrations tests, not here.
set -euo pipefail
PRIOR_TAG="${PRIOR_TAG:-p2-m6-possession-audits}"
PAIRING="${1:-all}"
WORK="$(mktemp -d)"; trap 'docker rm -f xv-pg xv-kubo >/dev/null 2>&1 || true; git worktree remove --force "$WORK/old" 2>/dev/null || true; rm -rf "$WORK"' EXIT

build_side() { # $1=dir $2=outprefix — builds coordinator, node, novactl, migrate
  (cd "$1" && go build -o "$WORK/bin/$2-coordinator" ./cmd/coordinator \
            && go build -o "$WORK/bin/$2-node"        ./cmd/node \
            && go build -o "$WORK/bin/$2-novactl"     ./cmd/novactl \
            && go build -o "$WORK/bin/$2-migrate"     ./cmd/migrate)
}

git worktree add "$WORK/old" "$PRIOR_TAG"
mkdir -p "$WORK/bin"
build_side .           head
build_side "$WORK/old" old

start_pg()   { docker run -d --name xv-pg -e POSTGRES_PASSWORD=nova -e POSTGRES_DB=nova_scratch -p 127.0.0.1:15544:5432 postgres:16; wait_pg; }
start_kubo() { docker run -d --name xv-kubo -p 127.0.0.1:15001:5001 ipfs/kubo:latest; wait_kubo; }
psqlq()      { docker exec xv-pg psql -U postgres -d nova_scratch -tAc "$1"; }
wait_sql()   { local q="$1" want="$2" n=0; until [ "$(psqlq "$q")" = "$want" ] || [ $n -gt 60 ]; do sleep 2; n=$((n+1)); done; [ "$(psqlq "$q")" = "$want" ]; }

run_pairing() { # $1=coord-side (head|old) $2=donor-side (head|old)
  start_pg; start_kubo
  "$WORK/bin/$1-migrate" up                                # coordinator's OWN schema ceiling
  "$WORK/bin/$1-novactl" node ca-init --dir "$WORK/ca" --coordinator-ip 127.0.0.1
  "$WORK/bin/$1-novactl" node issue --dir "$WORK/ca" --name xv-donor --out "$WORK/donor"
  # operator.yaml + node.yaml: generate from EACH side's own templates
  # ($2-novactl node nebula-template), then sed the loopback addrs + short
  # possession cadence (possession_audit.base_interval_seconds: 5).
  write_configs "$1" "$2"
  "$WORK/bin/$1-coordinator" --config "$WORK/operator.yaml" & COORD=$!
  "$WORK/bin/$2-node"        --config "$WORK/donor/node.yaml" & DONOR=$!
  # join:
  wait_sql "SELECT count(*) FROM nodes WHERE status='active' AND assignment_sync_state='current'" 1
  # serve: assign one pin via the coordinator-side novactl and await the ack
  seed_one_blob                                            # psql INSERT of a tiny fixture blob (helper below)
  "$WORK/bin/$1-novactl" pin assign --cid "$XV_CID" --node "$XV_NODE"
  wait_sql "SELECT count(*) FROM pin_assignments WHERE state='acked'" 1
  # audit: short cadence → one pass row
  wait_sql "SELECT count(*) FROM pin_audits WHERE result='pass'" 1
  if [ "$1" = head ]; then                                 # drain: HEAD coordinator ONLY
    "$WORK/bin/head-novactl" node drain --id "$XV_NODE" --no-confirm
    wait_sql "SELECT count(*) FROM nodes WHERE draining_at IS NOT NULL" 1
  fi
  kill $DONOR $COORD; docker rm -f xv-pg xv-kubo
}
case "$PAIRING" in
  head-head)            run_pairing head head ;;
  head-coord-old-donor) run_pairing head old  ;;
  old-coord-head-donor) run_pairing old  head ;;
  all)                  run_pairing head head; run_pairing head old; run_pairing old head ;;
esac
echo "OK: crossversion pairing(s) $PAIRING passed"
```

> **Implementation note (caution 2):** `write_configs` and `seed_one_blob` are the
> two helpers that must be filled against the REAL config schemas — generate each
> side's `node.yaml`/compose from **that side's own** `novactl node nebula-template`
> output (version-correct fields by construction) and sed in: loopback
> `coordinator_url`, the Kubo API address, the issued cert paths, and a writable
> `storage_dir`. If a field the template emits is Nebula-specific, point it at the
> loopback equivalents the e2e Go tests use (`internal/federation/e2e` shows the
> loopback-mTLS posture — no real Nebula needed). Expect this step to need 2–3
> iterations against real binaries; that is the point of the drill.

Makefile:

```make
.PHONY: crossversion-e2e
# P2-M7 D-M7-3: local gate; requires docker. PAIRING=all|head-head|...
crossversion-e2e:
	./scripts/crossversion_e2e.sh $(PAIRING)
```

- [ ] **Step 2: Run each pairing** — `make crossversion-e2e PAIRING=head-head`,
  then `head-coord-old-donor`, then `old-coord-head-donor`.
  Expected: `OK: crossversion pairing(s) … passed` for each. Fix `write_configs`
  iteratively until real binaries interoperate.

- [ ] **Step 3: Commit**

```bash
git add scripts/crossversion_e2e.sh Makefile
git commit -m "test(p2-m7): cross-version e2e — N−1×HEAD matrix, per-side schema ceilings, drain HEAD-only (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 10: Operational drills — automated proofs

**Files:**
- Create: `internal/orchestrator/drills_test.go`,
  `internal/node/transfer/outofspace_test.go`,
  `internal/node/state/corrupt_test.go`,
  `internal/federation/e2e/m7_drain_test.go`

**Interfaces:** consumes existing helpers — `ReconcileNodeLiveness`,
the projection/seed helpers in `internal/orchestrator/*_test.go`, the m5/m6 e2e
capstone helpers, `wire.FailReasonOutOfSpace`.

- [ ] **Step 1: Write the failing drills** (each maps 1:1 to a D-M7-4 row):

`internal/orchestrator/drills_test.go`:

```go
func TestDrillRevocationHeals(t *testing.T) {
	// Precondition: node A acked holder of cid X (target 2), healthy B holds X,
	// C is an eligible destination. Fault: UPDATE nodes SET status='revoked' (the
	// novactl path is DB-direct). Run ReconcileNodeLiveness → assert:
	//   - reconcile queue contains X with reason 'node_revoked'
	//   - revoked_signaled_at set (emit-once)
	//   - RecomputeReplicationCounts(X): healthy_acked == 1 (A dropped instantly)
	// NOT proven here: the transfer itself (m5 e2e owns repair transport).
}

func TestDrillProviderLossHeals(t *testing.T) {
	// Precondition: domains P1{A1,A2} P2{B1,B2} operator-verified; X acked on
	// A1+A2 ONLY (target 2); B1/B2 empty+eligible (enough surviving capacity —
	// the fixture must not degenerate into no-destination starvation).
	// Fault: backdate last_seen_at for A1,A2 beyond unreachable threshold.
	// Sweep → both unreachable; X enqueued; recompute → healthy 0 ⇒ donor_lost
	// tier visible; scheduler's next pick places to P2 (assert reservation row
	// pending on B1 or B2).
}
```

`internal/node/transfer/outofspace_test.go`:

```go
func TestDrillOutOfSpaceIsCleanRefusal(t *testing.T) {
	// Pinner fake returns an error wrapping syscall.ENOSPC. Verify(...) (or the
	// agent's fail-classification seam) must yield FailReason == wire.FailReasonOutOfSpace
	// and MUST NOT corrupt/partially-write local progress state.
}
```

> **Implementation note (caution 3):** if the current classifier maps ENOSPC to
> `FailReasonOther`, add the mapping in the donor's existing fail-reason
> classification (a `errors.Is(err, syscall.ENOSPC)` arm) — this is the one
> donor-side diff in M7, it is an error-path classification only, and
> `NormalizeFailReason` already accepts `out_of_space` on the wire.

`internal/node/state/corrupt_test.go`:

```go
func TestDrillCorruptStateFailsSafe(t *testing.T) {
	// registration.json truncated to garbage → FileRegistrationStore load returns
	// an error (fail-fast; NEVER silently re-register).
	// cursor/progress store corrupted → load path returns the sentinel that
	// drives snapshot_required recovery (assert the agent's recovery branch is
	// taken — mirror agent_sync_test.go's snapshot-recovery fixture).
}
```

`internal/federation/e2e/m7_drain_test.go` (capstone):

```go
func TestE2EDrainDecommission(t *testing.T) {
	// Real loopback-mTLS donors (reuse the m5 healing harness): A (draining
	// source) holds cid X acked; B empty destination; target 1→2 forces repair.
	// 1. drain A via the Task-3 SQL sequence (SetNodeDraining + fail-pendings +
	//    enqueue) — same statements the CLI core runs.
	// 2. Orchestrator tick: repair is sourced FROM A (assert the repair token's
	//    source == A: draining remains repair-sourceable) into B.
	// 3. B acks → CountDrainPendingCIDs(A) == 0 (drain-ready).
	// 4. Revoke A → healthy_acked(X) unchanged (B carries it): ZERO durability
	//    loss end-to-end.
}
```

- [ ] **Step 2: Run FAIL** — the four files' tests fail/undefined.

- [ ] **Step 3: Implement fixtures/assertions** (and the ENOSPC classifier arm if
  needed per caution 3).

- [ ] **Step 4: Run PASS**

Run: `go test ./internal/orchestrator/ ./internal/node/... ./internal/federation/e2e/ -run 'Drill|TestE2EDrain' -v -count=1`
Expected: PASS. Then the boundary: `./scripts/check_node_deps.sh` → clean.

- [ ] **Step 5: Commit**

```bash
git add internal/orchestrator/drills_test.go internal/node/transfer/outofspace_test.go \
        internal/node/state/corrupt_test.go internal/federation/e2e/m7_drain_test.go \
        internal/node/transfer
git commit -m "test(p2-m7): operational drills — revocation/provider-loss heal, out_of_space refusal, corrupt-state fail-safe, drain e2e capstone (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 11: Volunteer walkthrough + runbooks + doc drift fixes

**Files:**
- Modify: `docs/quickstart/donor.md`, `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`,
  `docs/VERSIONING.md`, `README.md`
- Create: `docs/runbooks/donor-lifecycle.md`, `docs/runbooks/failure-drills.md`

- [ ] **Step 1: Extract the REAL cosign identity/issuer from the workflow.** Open
  `.github/workflows/ci.yml` `donor-sbom-sign`. Keyless GitHub-Actions signatures
  carry: issuer `https://token.actions.githubusercontent.com`; certificate identity
  = the workflow ref URL. Derive the exact flags from the file (repo
  `nova-archive/nova`, workflow path `.github/workflows/ci.yml`, trusted ref
  `refs/heads/main`):

```sh
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/nova-archive/nova/\.github/workflows/ci\.yml@refs/heads/main$' \
  ghcr.io/nova-archive/nova-node@sha256:<digest>
```

**No invented values:** if the workflow file/job layout has changed by execution
time, re-derive from the file, and verify the regexp against a real signature
(`cosign verify … 2>&1 | head`) before documenting it.

- [ ] **Step 2: Rewrite `docs/quickstart/donor.md`** (replace the M1 stub; keep the
  "Release trust" framing). Required sections, in order: **Verify the image**
  (digest pin + the Step-1 `cosign verify` + `cosign verify-attestation --type
  spdxjson …` for the SBOM + provenance attestation verification via `gh attestation
  verify` or `cosign verify-attestation`); **Enroll** (operator issues the cert
  bundle — `novactl node issue` — Nebula enrollment per
  `VOLUNTEER_DEPLOYMENT_GUIDANCE.md`, annotated `node.yaml`); **Run** (compose from
  `deploy/donor/`, healthcheck); **Confirm you are serving** (operator: `novactl
  node list` shows `active`/`current`); **Upgrade / rollback** (digest re-pin, N−1
  donor interoperates with a HEAD coordinator per D-M7-3); **Leaving gracefully**
  (ask the operator to `drain`; keep the donor RUNNING until the operator confirms
  zero drain debt; then decommission — link the runbook).

- [ ] **Step 3: Write `docs/runbooks/donor-lifecycle.md`** — operator-executable
  procedures: **revoke vs suspend vs drain** (decision table: hostile/compromised →
  revoke now; questionable → suspend; leaving on good terms → drain);
  **graceful decommission** (drain → watch `nova_node_drain_pending_cids` +
  `nova_node_drain_inflight_cids` → the D-M7-6f "safe to revoke" 3-condition gate →
  revoke → teardown); **mistaken drain** (`undrain`); **below-floor debt** (what
  `nova_below_floor_replica_debt` means, why replicas stay countable until P2-M6.1,
  when to leave alone / drain / revoke).

- [ ] **Step 4: Write `docs/runbooks/failure-drills.md`** — provider loss (triage,
  what the concentration/`donor_lost` signals look like on `/metrics`, egress-budget
  expectations, when NOT to panic), disk full (donor frees space, rejoins; the
  `out_of_space` refusal is clean), corrupt donor state (cursor/progress: safe to
  delete → snapshot recovery; `registration.json`/cert material: never casually
  regenerate — operator re-issue path).

- [ ] **Step 5: Drift fixes** — `README.md`: Phase-2 status paragraph now says
  M1–M7 shipped, streaming-AEAD (M8–M10) next; `docs/VERSIONING.md` release
  checklist: add "cross-version `make crossversion-e2e` green; `make bench-corpus`
  artifact recorded"; `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`: point at the
  now-real quickstart + runbooks.

- [ ] **Step 6: Verify + commit**

Run: `python3 scripts/check_doc_links.py`
Expected: all links resolve.

```bash
git add docs/quickstart/donor.md docs/runbooks docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md docs/VERSIONING.md README.md
git commit -m "docs(p2-m7): volunteer walkthrough with workflow-extracted cosign policy + lifecycle/failure runbooks + drift fixes (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 12: Spec amendments + ROADMAP + master-plan flip

**Files:**
- Modify: `docs/specs/FEDERATION_PROTOCOL.md`, `docs/specs/HEALING_PROTOCOL.md`,
  `docs/specs/DATA_MODEL.sql`, `docs/specs/ARCHITECTURE_DECISIONS.md`,
  `docs/THREAT_MODEL.md`, `docs/ROADMAP.md`,
  `docs/superpowers/plans/phase2/2026-06-11-phase2-federation.md`

- [ ] **Step 1: Amend each normative spec** with the house blockquote convention
  (stacked under existing ones):
  `> **Amended by P2-M7 (2026-07-01) — implemented.** <what> (D-M7-N). See docs/superpowers/specs/phase2/2026-07-01-phase2-m7-production-hardening-release-design.md.`
  - `FEDERATION_PROTOCOL.md`: drain lifecycle (operator-side, not wire-visible;
    draining nodes keep serving read-source/repair-stream); the capability
    classification note (configured-required vs route-gated, D-M7-3).
  - `HEALING_PROTOCOL.md`: draining-node semantics — excluded from
    healthy/sourceable safety counts and placement; allowed, deprioritized, as
    read/repair source; drain-debt definition (D-M7-6b/6c/6f); explicit restatement
    that below-floor bulk re-replication remains **P2-M6.1**.
  - `DATA_MODEL.sql`: annotate `nodes.draining_at` + `nodes_draining_idx`.
  - `ARCHITECTURE_DECISIONS.md`: two rows — metrics plane (coordinator-only,
    dedicated listener, bounded labels; donor metrics deferred) and
    drain-vs-revoke lifecycle (voluntary vs involuntary departure).
  - `docs/THREAT_MODEL.md`: metrics-exposure note (loopback default, label
    discipline) + voluntary-vs-involuntary departure separation (drain cannot be
    set over the wire; a "draining" claim never inflates durability accounting).

- [ ] **Step 2: ROADMAP row.** Append the P2-M7 row to the additive-track table
  (mirror the M5/M6 row density): theme "Production hardening & donor release ✅",
  migration `0016`, tag `p2-m7-production-hardening-release`, design + plan paths,
  and **Deferrals:** below-floor bulk re-replication queue → **P2-M6.1**;
  `envelope_round_trip` + two-call audit → **P2-M8+**; donor-local metrics → later
  opt-in; multi-coordinator fencing → **Phase 6**.

- [ ] **Step 3: Master-plan status flip.** In
  `docs/superpowers/plans/phase2/2026-06-11-phase2-federation.md`, flip the P2-M7
  row from `pending | tbd` to done, linking this design/plan pair.

- [ ] **Step 4: Verify + commit**

Run: `python3 scripts/check_doc_links.py && make migrations-frozen`
Expected: clean.

```bash
git add docs/specs docs/THREAT_MODEL.md docs/ROADMAP.md docs/superpowers/plans/phase2/2026-06-11-phase2-federation.md
git commit -m "docs(p2-m7): FED/HEAL/DATA_MODEL/ARCH/THREAT amendments + ROADMAP P2-M7 row + master-plan flip (P2-M7)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Cross-cutting coverage notes

- **D-M7-1** (metrics): Tasks 4 (listener/families/config) + 5 (event-site hooks) +
  6 (donor deny) + 2 (`CountBelowFloorReplicas` feeding the debt gauge).
- **D-M7-1a** (type/source/reset): the Task-4 table comment in `metrics.go` is the
  normative in-code record; `TestScrapeFamiliesAndValues` pins the DB-derived set.
- **D-M7-2** (bench): Task 7. The FIRST full `make bench-corpus` run (release
  profile) happens at Final Verification and its artifact is committed.
- **D-M7-3** (compat): Tasks 8 (matrix) + 9 (cross-version). DB-upgrade coverage:
  Task 1's migration test (0001→0016 forward + the re-register semantics).
- **D-M7-4** (drills): Task 10 (proofs) + Task 11 (runbooks).
- **D-M7-5** (deferrals): enforced negatively — no task builds a below-floor queue
  or `envelope_round_trip`; Task 12 restates owners in ROADMAP/HEALING.
- **D-M7-6** (drain): Tasks 1 (column) + 2 (query classification + debt) + 3 (CLI)
  + 10 (drills/e2e) + 4 (drain metric families).
- **D-M7-7** (docs): Task 11. **D-M7-8**: every acceptance item maps to a test —
  1→T4, 2→T4 (`TestLabelDiscipline`), 3→T6, 4→T2+T4, 5→T7, 6→T8, 7→T9, 8→T10,
  9→T2+T10, 10→T2 (`TestWeightZeroIsNotDrain`), 11→T1+T3, 12→T11, 13→T1+T6.

## Final verification (before merge)

- [ ] `go build ./... && go vet ./...`
- [ ] `go test ./... -count=1` (unit + integration; testcontainers up)
- [ ] `make codegen-check` — no sqlc drift
- [ ] `make migrations-frozen` — `0016` appended, `0001–0015` untouched
- [ ] `./scripts/check_node_deps.sh` — clean; prometheus deny demonstrated red in Task 6
- [ ] `gofmt -l $(git diff --name-only main -- '*.go')` — empty
- [ ] `make bench-corpus-explain && make bench-corpus-ci` — green
- [ ] `make bench-corpus` — full release run; commit the
      `reports/benchmarks/p2-m7-corpus-<date>.{json,md}` artifact
- [ ] `make crossversion-e2e PAIRING=all` — three pairings green
- [ ] `python3 scripts/check_doc_links.py` — all links resolve
- [ ] Finish per the milestone workflow: local fast-forward merge to `main` +
      annotated tag `p2-m7-production-hardening-release`; **no remote push**.
