# P2-M3 Assignment Synchronization — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task, a fresh subagent per task with review between tasks. Each task is TDD: write the failing test, run it red, implement to green, then commit. Steps use checkbox (`- [ ]`) syntax for tracking. Tasks 2/4/5/6/7/8/14/15 require a test Postgres (`internal/dbtest` testcontainer).

**Goal:** Stand up Nova's assignment-synchronization control plane — a durable
`pin_changes` log with retention + snapshot recovery, generationed
`pin_assignments`, the coordinator `changes/snapshot/ack/fail` endpoints, the
advisory-locked `AssignPin`/`UnpinPin` transaction seam surfaced by `novactl pin
assign|unpin|list`, and a donor that durably converges a **desired-assignment
set** with crash + long-offline recovery — **with no byte transfer and no
production donor `ack`** (those are P2-M4).

**Architecture:** A new forward-only migration `0012` (the first non-frozen
Phase-2 migration) versions `pin_assignments` and adds `pin_changes` + a singleton
retention watermark. The existing `internal/federation/coordinator` mTLS server
(from M2) gains four `/fed/v1/pins/*` handlers and a retention goroutine; the
exported `coordinator.AssignPin`/`UnpinPin` seam (advisory-locked, single-txn) is
the **only** writer of assignment state and is reused by `novactl pin` and (later)
the M5 scheduler. The donor gains durable atomic-JSON local state (`FileStore`
cursor + `FileAssignmentStore` desired set) and a second poll loop over an
extended `Client`. `internal/federation/wire` is reconciled to the normative
protocol JSON. No byte transfer, no repair tokens, no donor `ack` — M4.

**Tech Stack:** Go (stdlib `net/http` 1.22+ routing + `r.PathValue`, `log/slog`,
`crypto`), pgx/v5 + pgxpool + sqlc (`pgx/v5`, `emit_interface`), goose migrations,
`github.com/google/uuid`, `internal/dbtest` (Postgres 16 testcontainer).

**Design:** [`../../specs/phase2/2026-06-22-phase2-m3-assignment-sync-design.md`](../../specs/phase2/2026-06-22-phase2-m3-assignment-sync-design.md)

---

## Preconditions (exist from M0–M2; do not re-create)

- `internal/federation/transport`: `IdentityFromCert`, `Identity`, `ClientTLSConfig`, `ServerTLSConfig`, `NewTLSListener`.
- `internal/federation/coordinator`: `Server{q *gen.Queries, cfg Config, …}`, `New(q, cfg)`, `Config{ListenAddr, RequiredCapabilities, Timers wire.ConfigUpdates, TLS}`, `authenticate`, `writeJSON(w,status,v)`, `writeError(w,status,code,msg)`, `mux()`, `pgUUIDFrom`, `pgText`, `pgInt8`, `Listen`, `Run`, `Addr`. Test helpers in `register_test.go`/`heartbeat_test.go`: `newTestServer`, `reqWithCert`, `issuedClient`, `registerOK`, `pemDecode`.
- `internal/federation/wire`: `ProtocolV1`, `CapPinChangeLog`, `CapSnapshot`, `CodeSnapshotRequired`, `CodeUnknownChangeKind`, `ConfigUpdates{HeartbeatIntervalSeconds, PinsPollIntervalSeconds, MaxPinConcurrency}`, `RegisterRequest/Response`, `Heartbeat*`, `ChangesRequest/Response`, `PinChange`, `Ack`, `Fail`, `NegotiateCapabilities`, `ErrorResponse`.
- `internal/db/gen`: `Queries`, `New(DBTX)`, `(*Queries).WithTx(pgx.Tx)`, `RegisterNode`, `GetNodeByID`, `UpdateNodeHeartbeat`, `RevokeNode`; `PinState` + `PinStatePending/Acked/Failed/Unpinning`; `Node`, `PinAssignment`, `NodeStatusActive/Revoked`. Tables `pin_assignments (cid, node_id, state, assigned_at, acked_at; PK (cid,node_id))`, `blobs (cid PK, mime_type, byte_size, …)`, `nodes`; existing index `pin_assignments_cid_state_idx (cid, state)`.
- `internal/db`: `Open(ctx, dsn) (*pgxpool.Pool, error)`; `internal/dbtest`: `New(t, ctx) *pgxpool.Pool`.
- `internal/node/state`: `Store{Cursor,SetCursor,SeenJTI,RecordJTI}` + `MemStore`; `RegistrationStore` + `FileRegistrationStore` + `Registration` + `fsyncDir`.
- `internal/node/agent`: `Agent`, `New(cfg, regStore, client, interval)`, `Client{Register,Heartbeat}`, `HTTPClient`, `NewHTTPClient(base, *tls.Config)`.
- `cmd/novactl/node_db.go`: `withNodeDB(fn func(ctx, *gen.Queries) error) error`, `parsePGUUID`.

## File structure

**Schema / queries (regenerate with `make sqlc-generate`):**
- `internal/db/migrations/0012_assignment_sync.sql` — *create*.
- `internal/db/migrations/MANIFEST.sha256` — *modify*: append `0012` hash.
- `internal/db/queries/federation.sql` — *modify*: append the M3 queries.
- `internal/db/gen/*` — *regenerate*.

**Coordinator (operator-only):**
- `internal/federation/coordinator/assignments.go` — *create*: `Assignment`, `AssignPin`, `UnpinPin`.
- `internal/federation/coordinator/pins.go` — *create*: `handleChanges`, `handleSnapshot`, `handleAck`, `handleFail` + query helpers.
- `internal/federation/coordinator/retention.go` — *create*: `pruneOnce`, `runRetention`.
- `internal/federation/coordinator/server.go` — *modify*: register routes; start retention goroutine; `Config` gains `ChangeLogRetention`, `PrunePollInterval`.
- `internal/federation/coordinator/handlers.go` — *modify*: real `current_epoch` in `handleHeartbeat`.

**Shared:**
- `internal/federation/wire/messages.go` — *modify*: reconcile to the protocol JSON; add snapshot types + `CodeStaleAssignment`.

**Donor-only:**
- `internal/node/state/store_file.go` — *create*: `FileStore` (durable cursor; in-mem jti).
- `internal/node/state/assignments.go` — *create*: `AssignmentStore`, `DesiredAssignment`, `ChangeInput`, `FileAssignmentStore`.
- `internal/node/agent/client.go` — *modify*: `GetChanges`, `GetSnapshot`, sentinel errors.
- `internal/node/agent/agent.go` — *modify*: extend `Client`; new `New` signature; heartbeat + pins-poll loop; `syncOnce`/`recoverSnapshot`.
- `cmd/node/main.go` — *modify*: construct the file stores; wire the poll interval.

**CLI:**
- `cmd/novactl/pin.go` — *create*: `cmdPin` (`assign|unpin|list`).
- `cmd/novactl/node_db.go` — *modify*: add `withNodeDBPool`.
- `cmd/novactl/main.go` — *modify*: dispatch `pin` + `usage()`.

**Docs/status:** `ROADMAP.md` + master-design milestone table — *modify*.

---

## Task 1: Reconcile the shared `wire` types to the protocol JSON

**Files:**
- Modify: `internal/federation/wire/messages.go`
- Test: `internal/federation/wire/pins_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/federation/wire/pins_test.go`:

```go
package wire

import (
	"encoding/json"
	"testing"
)

func TestPinChangeTags(t *testing.T) {
	b, _ := json.Marshal(PinChange{Sequence: 7, AssignmentID: "a", Generation: 2, Kind: "assign", CID: "bafy", ByteSize: 1048576})
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"seq", "assignment_id", "generation", "kind", "cid", "byte_size"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("PinChange missing %q in %s", k, b)
		}
	}
	if _, ok := m["source"]; ok {
		t.Fatalf("source must be omitted when nil: %s", b)
	}
}

func TestChangesResponseTags(t *testing.T) {
	b, _ := json.Marshal(ChangesResponse{Changes: []PinChange{}, NextSeq: 9, CurrentEpoch: 12})
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)
	for _, k := range []string{"changes", "next_seq", "current_epoch"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("ChangesResponse missing %q in %s", k, b)
		}
	}
}

func TestSnapshotResponseRoundTrip(t *testing.T) {
	in := SnapshotResponse{
		Data:          []SnapshotItem{{CID: "bafy", AssignmentID: "a", Generation: 1, ByteSize: 5}},
		Cursor:        "bafy",
		SnapshotEpoch: 421,
	}
	b, _ := json.Marshal(in)
	var out SnapshotResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.SnapshotEpoch != 421 || len(out.Data) != 1 || out.Data[0].AssignmentID != "a" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestAckFailExtraFieldsAndCode(t *testing.T) {
	b, _ := json.Marshal(Ack{AssignmentID: "a", Generation: 1, CID: "bafy", ByteSize: 5, IPFSPinStatus: "pinned", FetchedFromNodeID: "n"})
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)
	for _, k := range []string{"byte_size", "ipfs_pin_status", "fetched_from_node_id"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("Ack missing %q", k)
		}
	}
	if CodeStaleAssignment == "" {
		t.Fatal("CodeStaleAssignment must be defined")
	}
	if FailReasonOutOfSpace == "" {
		t.Fatal("Fail reason constants must be defined")
	}
	if NormalizeFailReason("") != FailReasonOther || NormalizeFailReason("bogus") != "" {
		t.Fatal(`NormalizeFailReason: ""→other, unknown→""`)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/wire/ -run 'Pin|Changes|Snapshot|AckFail' -v`
Expected: FAIL — `ByteSize`/`NextSeq`/`SnapshotItem`/`SnapshotResponse`/`CodeStaleAssignment`/`FailReason*` undefined, and `seq` tag missing.

- [ ] **Step 3: Edit `internal/federation/wire/messages.go`**

Replace the M1 pin stubs (`ChangesResponse`, `PinChange`, `Ack`, `Fail`) and add the new types/constants. `ChangeSource` is **nil in M3** (M4 populates it), so no second wire bump is needed when repair tokens land.

```go
const CodeStaleAssignment = "stale_assignment" // ack/fail for a superseded generation

// Fail.Reason domain (FEDERATION_PROTOCOL.md).
const (
	FailReasonOutOfSpace        = "out_of_space"
	FailReasonBlobUnavailable   = "blob_unavailable"
	FailReasonPolicyFilter      = "policy_filter"
	FailReasonNetworkError      = "network_error"
	FailReasonKuboError         = "kubo_error"
	FailReasonSourceUnauthorized = "source_unauthorized"
	FailReasonCIDMismatch       = "cid_mismatch"
	FailReasonBudgetExceeded    = "budget_exceeded"
	FailReasonOther             = "other"
)

// NormalizeFailReason maps "" to FailReasonOther and returns "" for an
// unrecognized reason (the /fail handler rejects that with 400).
func NormalizeFailReason(r string) string {
	switch r {
	case "":
		return FailReasonOther
	case FailReasonOutOfSpace, FailReasonBlobUnavailable, FailReasonPolicyFilter,
		FailReasonNetworkError, FailReasonKuboError, FailReasonSourceUnauthorized,
		FailReasonCIDMismatch, FailReasonBudgetExceeded, FailReasonOther:
		return r
	default:
		return ""
	}
}

// Change kinds. Donors fail closed on any other value (D7).
const (
	ChangeKindAssign = "assign"
	ChangeKindUnpin  = "unpin"
)

// ChangeSource is the repair-fetch source for an assign change. Populated in M4
// (repair tokens); nil in M3.
type ChangeSource struct {
	NodeID     string `json:"node_id"`
	NebulaAddr string `json:"nebula_addr"`
	Token      string `json:"token"`
}

type PinChange struct {
	Sequence     int64         `json:"seq"`
	AssignmentID string        `json:"assignment_id"`
	Generation   int64         `json:"generation"`
	Kind         string        `json:"kind"`
	CID          string        `json:"cid"`
	ByteSize     int64         `json:"byte_size"`
	Source       *ChangeSource `json:"source,omitempty"` // M4
}

type ChangesResponse struct {
	Changes      []PinChange `json:"changes"`
	NextSeq      int64       `json:"next_seq"`
	CurrentEpoch int64       `json:"current_epoch"`
}

// SnapshotItem is one row of the recovery snapshot.
type SnapshotItem struct {
	CID          string `json:"cid"`
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	ByteSize     int64  `json:"byte_size"`
	AssignedAt   string `json:"assigned_at"` // RFC3339
}

type SnapshotResponse struct {
	Data          []SnapshotItem `json:"data"`
	Cursor        string         `json:"cursor"`         // empty ⇒ last page
	SnapshotEpoch int64          `json:"snapshot_epoch"`
}

type Ack struct {
	AssignmentID      string `json:"assignment_id"`
	Generation        int64  `json:"generation"`
	CID               string `json:"cid"`
	ByteSize          int64  `json:"byte_size,omitempty"`
	IPFSPinStatus     string `json:"ipfs_pin_status,omitempty"`
	FetchedFromNodeID string `json:"fetched_from_node_id,omitempty"`
}

type Fail struct {
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	CID          string `json:"cid"`
	Reason       string `json:"reason"`
	Details      string `json:"details,omitempty"`
}
```

Keep `ChangesRequest` as-is (the donor builds the query string from it).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/wire/ -v`
Expected: PASS (existing `capability_test.go`/`token_test.go`/`messages_test.go` still green).

- [ ] **Step 5: Commit**

```bash
git add internal/federation/wire/messages.go internal/federation/wire/pins_test.go
git commit -m "feat(p2-m3): reconcile fed/v1 pins wire types to the protocol (P2-M3)"
```

---

## Task 2: Migration `0012` + queries + sqlc regen

**Files:**
- Create: `internal/db/migrations/0012_assignment_sync.sql`
- Modify: `internal/db/migrations/MANIFEST.sha256`
- Modify: `internal/db/queries/federation.sql`
- Regenerate: `internal/db/gen/*`
- Test: `internal/db/migrations/migrations_test.go` (extend)

- [ ] **Step 1: Write the failing test**

Append to the migrations test a post-migration schema assertion (mirror the existing dbtest-backed style). If the file gates on `TEST_DATABASE_URL`, follow that pattern.

```go
func TestMigration0012AssignmentSync(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx) // applies all migrations

	// pin_assignments gained assignment_id + generation
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
	    WHERE table_name='pin_assignments' AND column_name IN ('assignment_id','generation')`).Scan(&n)
	if err != nil || n != 2 {
		t.Fatalf("pin_assignments columns: n=%d err=%v", n, err)
	}

	// pin_changes exists and is empty; watermark seeded to 0
	var head, wm int64
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(sequence),0) FROM pin_changes`).Scan(&head); err != nil {
		t.Fatalf("pin_changes: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT pruned_through_seq FROM federation_change_log_state`).Scan(&wm); err != nil {
		t.Fatalf("watermark: %v", err)
	}
	if head != 0 || wm != 0 {
		t.Fatalf("head=%d wm=%d, want 0/0", head, wm)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/migrations/ -run 0012 -v` (with a test Postgres)
Expected: FAIL — relations/columns do not exist.

- [ ] **Step 3: Create the migration**

`internal/db/migrations/0012_assignment_sync.sql`:

```sql
-- +goose Up
-- +goose StatementBegin
-- P2-M3: assignment versioning + durable change log (D6/D7). Sync-only and
-- read-selection-ready: blob_replication_state, the nodes D8/D9 placement
-- columns, and pin_audits receive-time columns land with their owning milestones
-- (M5/M6), not here. (cid, node_id) stays the natural current-assignment key;
-- assignment_id is the immutable handle carried in the change log + (M4) repair
-- tokens + acks.
ALTER TABLE pin_assignments
    ADD COLUMN assignment_id uuid   NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN generation    bigint NOT NULL DEFAULT 1,
    ADD CONSTRAINT pin_assignments_assignment_id_key UNIQUE (assignment_id);

CREATE TABLE pin_changes (
    sequence      bigserial PRIMARY KEY,
    node_id       uuid   NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    assignment_id uuid   NOT NULL,
    generation    bigint NOT NULL,
    kind          text   NOT NULL CHECK (kind IN ('assign', 'unpin')),
    cid           text   NOT NULL,
    byte_size     bigint NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pin_changes_node_seq_idx ON pin_changes (node_id, sequence);

-- Singleton retention watermark: the highest sequence pruned out of pin_changes.
-- A donor whose since_seq < pruned_through_seq must recover via snapshot (D7).
CREATE TABLE federation_change_log_state (
    id                 boolean PRIMARY KEY DEFAULT true CHECK (id),
    pruned_through_seq bigint  NOT NULL DEFAULT 0
);
INSERT INTO federation_change_log_state (id) VALUES (true);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE federation_change_log_state;
DROP TABLE pin_changes;
ALTER TABLE pin_assignments
    DROP CONSTRAINT pin_assignments_assignment_id_key,
    DROP COLUMN generation,
    DROP COLUMN assignment_id;
-- +goose StatementEnd
```

- [ ] **Step 4: Append the M3 queries**

Append to `internal/db/queries/federation.sql`:

```sql
-- name: GetChangeLogHead :one
SELECT COALESCE(MAX(sequence), 0)::bigint AS head FROM pin_changes;

-- name: GetPruneWatermark :one
SELECT pruned_through_seq FROM federation_change_log_state WHERE id = true;

-- name: AcquireChangeLogLock :exec
-- Transaction-scoped advisory lock that serializes change-log appends so
-- pin_changes.sequence values commit in assignment order (commit-order-safe):
-- a donor never advances its cursor past a lower-sequence row that can still
-- commit. Gaps from rolled-back txns are harmless (cursor uses sequence > N).
SELECT pg_advisory_xact_lock(8030600000000000001);

-- name: GetBlobSize :one
SELECT byte_size FROM blobs WHERE cid = $1;

-- name: UpsertPinAssignmentAssign :one
INSERT INTO pin_assignments (cid, node_id, state, generation)
VALUES ($1, $2, 'pending', 1)
ON CONFLICT (cid, node_id) DO UPDATE SET
    state       = 'pending',
    generation  = pin_assignments.generation + 1,
    acked_at    = NULL,
    assigned_at = now()
RETURNING assignment_id, generation;

-- name: GetPinAssignmentForUpdate :one
SELECT assignment_id, generation FROM pin_assignments
WHERE cid = $1 AND node_id = $2
FOR UPDATE;

-- name: DeletePinAssignment :execrows
DELETE FROM pin_assignments WHERE cid = $1 AND node_id = $2;

-- name: InsertPinChange :one
INSERT INTO pin_changes (node_id, assignment_id, generation, kind, cid, byte_size)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING sequence;

-- name: GetPinChangesSince :many
SELECT sequence, assignment_id, generation, kind, cid, byte_size
FROM pin_changes
WHERE node_id = $1 AND sequence > $2
ORDER BY sequence
LIMIT $3;

-- name: NodeHasChangesAfter :one
SELECT EXISTS (
    SELECT 1 FROM pin_changes WHERE node_id = $1 AND sequence > $2
) AS changed;

-- name: GetPinSnapshotPage :many
SELECT pa.cid, pa.assignment_id, pa.generation, b.byte_size, pa.assigned_at
FROM pin_assignments pa
JOIN blobs b ON b.cid = pa.cid
WHERE pa.node_id = $1 AND pa.cid > $2
ORDER BY pa.cid
LIMIT $3;

-- name: AckPinAssignment :execrows
UPDATE pin_assignments
SET state = 'acked', acked_at = now()
WHERE cid = $1 AND node_id = $2 AND assignment_id = $3 AND generation = $4 AND state = 'pending';

-- name: FailPinAssignment :execrows
UPDATE pin_assignments
SET state = 'failed'
WHERE cid = $1 AND node_id = $2 AND assignment_id = $3 AND generation = $4 AND state = 'pending';

-- name: GetPinAssignment :one
SELECT cid, node_id, state, assignment_id, generation, assigned_at, acked_at
FROM pin_assignments
WHERE cid = $1 AND node_id = $2;

-- name: PruneChangeLog :one
-- Atomic delete + watermark advance in one statement (no tx needed).
WITH del AS (
    DELETE FROM pin_changes WHERE created_at < $1 RETURNING sequence
)
UPDATE federation_change_log_state
SET pruned_through_seq = GREATEST(
    pruned_through_seq,
    COALESCE((SELECT MAX(sequence) FROM del), pruned_through_seq)
)
WHERE id = true
RETURNING pruned_through_seq;

-- name: ListDesiredAssignmentsByCID :many
SELECT node_id, generation, state FROM pin_assignments WHERE cid = $1 ORDER BY node_id;

-- name: ListVerifiedHoldersByCID :many
SELECT node_id, generation FROM pin_assignments WHERE cid = $1 AND state = 'acked' ORDER BY node_id;

-- name: ListDesiredAssignmentsByNode :many
SELECT cid, generation, state FROM pin_assignments WHERE node_id = $1 ORDER BY cid;
```

- [ ] **Step 5: Regenerate sqlc + append manifest**

```bash
make sqlc-generate
(cd internal/db/migrations && sha256sum 0012_assignment_sync.sql >> MANIFEST.sha256)
```

> The generated identifiers (`gen.GetPinChangesSinceParams`/`Row`, `gen.GetPinSnapshotPageParams`/`Row`, `gen.UpsertPinAssignmentAssignParams`/`Row`, `gen.AckPinAssignmentParams`, `gen.InsertPinChangeParams`, `gen.PruneChangeLog`, etc.) are produced here; later tasks must match the actual generated names (flagged inline).

- [ ] **Step 6: Verify codegen + frozen gate + migration applies**

```bash
go build ./internal/db/...
make migrations-frozen
go test ./internal/db/migrations/ -v   # with a test Postgres
```
Expected: build OK; `migrations-frozen` success (only `0012` added, manifest appended); migration test PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/db/migrations/0012_assignment_sync.sql internal/db/migrations/MANIFEST.sha256 internal/db/queries/federation.sql internal/db/gen/ internal/db/migrations/migrations_test.go
git commit -m "feat(p2-m3): migration 0012 assignment_sync + change-log queries (P2-M3)"
```

---

## Task 3: Assignment transaction seam (`AssignPin` / `UnpinPin`)

**Files:**
- Create: `internal/federation/coordinator/assignments.go`
- Test: `internal/federation/coordinator/assignments_test.go`

- [ ] **Step 1: Write the failing test**

This is a dbtest. Add a shared `seedBlob` helper here (reused by later tasks).

```go
package coordinator

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
)

func seedBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string, size int64) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO blobs (cid, mime_type, byte_size) VALUES ($1,'application/octet-stream',$2)`, cid, size)
	if err != nil {
		t.Fatal(err)
	}
}

func seedNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint, capacity_bytes, bandwidth_budget_bytes_per_day)
	    VALUES ($1,$2,$3,0,0)`, pgtype.UUID{Bytes: id, Valid: true}, "neb:"+id.String(), "fed:"+id.String())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAssignPinCreatesRowAndChange(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	node := seedNode(t, ctx, pool)
	seedBlob(t, ctx, pool, "bafy1", 1048576)

	tx, _ := pool.Begin(ctx)
	a, err := AssignPin(ctx, tx, "bafy1", node)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Generation != 1 || a.AssignmentID == uuid.Nil || a.Sequence < 1 {
		t.Fatalf("assign #1: %+v", a)
	}

	// re-assign bumps generation, keeps assignment_id
	tx2, _ := pool.Begin(ctx)
	a2, err := AssignPin(ctx, tx2, "bafy1", node)
	if err != nil {
		t.Fatal(err)
	}
	tx2.Commit(ctx)
	if a2.Generation != 2 || a2.AssignmentID != a.AssignmentID {
		t.Fatalf("assign #2 should bump gen, keep id: %+v (was %+v)", a2, a)
	}

	// one assign change row per call
	var changes int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_changes WHERE cid='bafy1' AND kind='assign'`).Scan(&changes)
	if changes != 2 {
		t.Fatalf("assign changes = %d, want 2", changes)
	}
}

func TestUnpinPinWritesChangeAndDeletesRow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	node := seedNode(t, ctx, pool)
	seedBlob(t, ctx, pool, "bafy1", 10)

	tx, _ := pool.Begin(ctx)
	a, _ := AssignPin(ctx, tx, "bafy1", node)
	tx.Commit(ctx)

	tx2, _ := pool.Begin(ctx)
	u, err := UnpinPin(ctx, tx2, "bafy1", node)
	if err != nil {
		t.Fatal(err)
	}
	tx2.Commit(ctx)
	if u.Generation != a.Generation+1 {
		t.Fatalf("unpin gen = %d, want %d", u.Generation, a.Generation+1)
	}

	var rows int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_assignments WHERE cid='bafy1'`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("row should be deleted, got %d", rows)
	}
	var unpins int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_changes WHERE cid='bafy1' AND kind='unpin'`).Scan(&unpins)
	if unpins != 1 {
		t.Fatalf("unpin changes = %d, want 1", unpins)
	}
}

func TestAssignSequencesCommitOrderedUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	node := seedNode(t, ctx, pool)
	for i := 0; i < 20; i++ {
		seedBlob(t, ctx, pool, cidN(i), 1)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, _ := pool.Begin(ctx)
			if _, err := AssignPin(ctx, tx, cidN(i), node); err != nil {
				tx.Rollback(ctx)
				return
			}
			tx.Commit(ctx)
		}(i)
	}
	wg.Wait()
	// All 20 commit (no rollbacks here), so the advisory lock yields a
	// contiguous, commit-ordered 1..20. The invariant under test is commit
	// ordering — a donor never advances its cursor past a lower-sequence row that
	// can still commit; gaps from rolled-back txns (none here) are harmless.
	var lo, hi, cnt int64
	pool.QueryRow(ctx, `SELECT min(sequence), max(sequence), count(*) FROM pin_changes`).Scan(&lo, &hi, &cnt)
	if cnt != 20 || hi-lo != 19 {
		t.Fatalf("sequences not commit-ordered: lo=%d hi=%d cnt=%d", lo, hi, cnt)
	}
}

func cidN(i int) string { return "bafy" + string(rune('a'+i)) }
```

> Note: the test bodies use direct `pool.Begin(ctx)` / `tx.Commit(ctx)`; `tx` is `pgx.Tx`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/coordinator/ -run 'AssignPin|UnpinPin|CommitOrdered' -v`
Expected: FAIL — `AssignPin`/`UnpinPin`/`Assignment` undefined.

- [ ] **Step 3: Create `internal/federation/coordinator/assignments.go`**

```go
package coordinator

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// Assignment is the result of a mutation: the immutable handle, its current
// generation, and the change-log sequence emitted.
type Assignment struct {
	AssignmentID uuid.UUID
	Generation   int64
	Sequence     int64
}

// AssignPin makes (cid,node) a desired assignment: it upserts pin_assignments
// (new assignment_id + generation 1 on first assign; generation++ on re-assign,
// keeping the immutable assignment_id) and appends an 'assign' change. The
// advisory lock serializes change-log appends so sequences commit in assignment
// order (commit-order-safe). Caller owns the tx (novactl/tests in M3; the M5
// scheduler later).
func AssignPin(ctx context.Context, tx pgx.Tx, cid string, nodeID uuid.UUID) (Assignment, error) {
	q := gen.New(tx)
	if err := q.AcquireChangeLogLock(ctx); err != nil {
		return Assignment{}, err
	}
	size, err := q.GetBlobSize(ctx, cid)
	if err != nil {
		return Assignment{}, err // unknown blob ⇒ rollback, no orphan
	}
	up, err := q.UpsertPinAssignmentAssign(ctx, gen.UpsertPinAssignmentAssignParams{
		Cid: cid, NodeID: pgUUIDFrom(nodeID),
	})
	if err != nil {
		return Assignment{}, err
	}
	seq, err := q.InsertPinChange(ctx, gen.InsertPinChangeParams{
		NodeID: pgUUIDFrom(nodeID), AssignmentID: up.AssignmentID,
		Generation: up.Generation, Kind: wire.ChangeKindAssign, Cid: cid, ByteSize: size,
	})
	if err != nil {
		return Assignment{}, err
	}
	slog.Info("fed.assign.txn", "cid", cid, "node_id", nodeID, "generation", up.Generation, "seq", seq)
	return Assignment{AssignmentID: up.AssignmentID.Bytes, Generation: up.Generation, Sequence: seq}, nil
}

// UnpinPin retires a desired assignment: it appends an 'unpin' change at the next
// generation and deletes the live row.
func UnpinPin(ctx context.Context, tx pgx.Tx, cid string, nodeID uuid.UUID) (Assignment, error) {
	q := gen.New(tx)
	if err := q.AcquireChangeLogLock(ctx); err != nil {
		return Assignment{}, err
	}
	cur, err := q.GetPinAssignmentForUpdate(ctx, gen.GetPinAssignmentForUpdateParams{Cid: cid, NodeID: pgUUIDFrom(nodeID)})
	if err != nil {
		return Assignment{}, err // pgx.ErrNoRows ⇒ not assigned
	}
	nextGen := cur.Generation + 1
	seq, err := q.InsertPinChange(ctx, gen.InsertPinChangeParams{
		NodeID: pgUUIDFrom(nodeID), AssignmentID: cur.AssignmentID,
		Generation: nextGen, Kind: wire.ChangeKindUnpin, Cid: cid, ByteSize: 0,
	})
	if err != nil {
		return Assignment{}, err
	}
	if _, err := q.DeletePinAssignment(ctx, gen.DeletePinAssignmentParams{Cid: cid, NodeID: pgUUIDFrom(nodeID)}); err != nil {
		return Assignment{}, err
	}
	slog.Info("fed.unpin.txn", "cid", cid, "node_id", nodeID, "generation", nextGen, "seq", seq)
	return Assignment{AssignmentID: cur.AssignmentID.Bytes, Generation: nextGen, Sequence: seq}, nil
}
```

> Match the generated field/param names from Task 2 (e.g. `up.AssignmentID` is `pgtype.UUID`; `.Bytes` yields the `[16]byte` assignable to `uuid.UUID`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/coordinator/ -run 'AssignPin|UnpinPin|CommitOrdered' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/federation/coordinator/assignments.go internal/federation/coordinator/assignments_test.go
git commit -m "feat(p2-m3): advisory-locked AssignPin/UnpinPin transaction seam (P2-M3)"
```

---

## Task 4: `handleChanges` + route

**Files:**
- Create: `internal/federation/coordinator/pins.go`
- Modify: `internal/federation/coordinator/server.go` (routes)
- Test: `internal/federation/coordinator/changes_test.go`

- [ ] **Step 1: Write the failing test**

Add a `newTestServerPool` helper (returns the pool so tests can seed via `AssignPin`). Reuse `registerOK`/`reqWithCert` from the M2 test files.

```go
func newTestServerPool(t *testing.T) (*Server, *pgxpool.Pool, []byte, []byte) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	s := New(gen.New(pool), Config{Timers: wireTimers()})
	return s, pool, caPEM, caKeyPEM
}

func assignViaSeam(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string, node uuid.UUID) {
	t.Helper()
	tx, _ := pool.Begin(ctx)
	if _, err := AssignPin(ctx, tx, cid, node); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestChangesEmptyThenRows(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	// empty: no changes, next_seq=0, current_epoch=0
	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	var resp wire.ChangesResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != 200 || len(resp.Changes) != 0 || resp.CurrentEpoch != 0 {
		t.Fatalf("empty changes: code=%d resp=%+v", w.Code, resp)
	}

	seedBlob(t, ctx, pool, "bafy1", 5)
	assignViaSeam(t, ctx, pool, "bafy1", id)

	w = httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Changes) != 1 || resp.Changes[0].Kind != "assign" || resp.Changes[0].CID != "bafy1" || resp.Changes[0].ByteSize != 5 {
		t.Fatalf("changes after assign: %+v", resp)
	}
	if resp.NextSeq != resp.Changes[0].Sequence || resp.CurrentEpoch != resp.Changes[0].Sequence {
		t.Fatalf("next_seq/epoch: %+v", resp)
	}
}

func TestChangesSnapshotRequiredBelowWatermark(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	// advance the watermark directly
	pool.Exec(ctx, `UPDATE federation_change_log_state SET pruned_through_seq=100 WHERE id=true`)

	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=50", nil, leaf))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	var er wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &er)
	if er.Code != wire.CodeSnapshotRequired {
		t.Fatalf("code = %q", er.Code)
	}
}

func TestChangesRevokedNode403(t *testing.T) {
	s, _, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	s.q.RevokeNode(context.Background(), pgUUIDFrom(id))
	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
}
```

Add the `wireTimers()` helper if not already shared: `func wireTimers() wire.ConfigUpdates { return wire.ConfigUpdates{HeartbeatIntervalSeconds: 300, PinsPollIntervalSeconds: 600, MaxPinConcurrency: 16} }`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/coordinator/ -run Changes -v`
Expected: FAIL — `handleChanges` undefined.

- [ ] **Step 3: Create `internal/federation/coordinator/pins.go`** (changes handler + shared helpers)

```go
package coordinator

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
)

const defaultPinPageLimit = 1000

// authNode authenticates the peer, parses its node UUID, and rejects revoked
// nodes — the shared front-half of every /fed/v1/pins/* handler.
func (s *Server) authNode(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return uuid.Nil, false
	}
	nodeUUID, err := uuid.Parse(id.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_node_id", "")
		return uuid.Nil, false
	}
	node, err := s.q.GetNodeByID(r.Context(), pgUUIDFrom(nodeUUID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "registration_required", "node must register first")
		return uuid.Nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "lookup failed")
		return uuid.Nil, false
	}
	if node.Status == gen.NodeStatusRevoked {
		writeError(w, http.StatusForbidden, "node_revoked", "")
		return uuid.Nil, false
	}
	if node.FederationCertFingerprint != id.Fingerprint {
		writeError(w, http.StatusForbidden, "fingerprint_mismatch", "presented cert is not the active cert")
		return uuid.Nil, false
	}
	return nodeUUID, true
}

// queryInt parses a non-negative int64 query param. Absent ⇒ def. Present but
// malformed or negative ⇒ error (the caller returns 400 bad_request).
func queryInt(r *http.Request, key string, def int64) (int64, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return n, nil
}

func clampLimit(n int64) int32 {
	if n < 1 || n > defaultPinPageLimit {
		return defaultPinPageLimit
	}
	return int32(n)
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	node, ok := s.authNode(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	since, err := queryInt(r, "since_seq", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	limRaw, err := queryInt(r, "limit", defaultPinPageLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	limit := clampLimit(limRaw)

	wm, err := s.q.GetPruneWatermark(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "watermark")
		return
	}
	if since < wm {
		writeError(w, http.StatusBadRequest, wire.CodeSnapshotRequired, "since_seq predates retention")
		return
	}
	rows, err := s.q.GetPinChangesSince(ctx, gen.GetPinChangesSinceParams{
		NodeID: pgUUIDFrom(node), Sequence: since, Limit: limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "changes")
		return
	}
	head, err := s.q.GetChangeLogHead(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "head")
		return
	}
	changes := make([]wire.PinChange, len(rows))
	for i, row := range rows {
		changes[i] = wire.PinChange{
			Sequence: row.Sequence, AssignmentID: uuid.UUID(row.AssignmentID.Bytes).String(),
			Generation: row.Generation, Kind: row.Kind, CID: row.Cid, ByteSize: row.ByteSize,
		}
	}
	next := head
	if int64(len(rows)) == int64(limit) && len(rows) > 0 {
		next = rows[len(rows)-1].Sequence // full page: more may exist
	}
	slog.Info("fed.changes.served", "node_id", node, "since_seq", since, "returned", len(rows), "next_seq", next)
	writeJSON(w, http.StatusOK, wire.ChangesResponse{Changes: changes, NextSeq: next, CurrentEpoch: head})
}
```

Register the route in `server.go`'s `mux()` (imports `fmt`/`log/slog` are already in the block above):

```go
m.HandleFunc("GET /fed/v1/pins/changes", s.handleChanges)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/coordinator/ -run Changes -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/federation/coordinator/pins.go internal/federation/coordinator/server.go internal/federation/coordinator/changes_test.go
git commit -m "feat(p2-m3): GET /fed/v1/pins/changes handler (P2-M3)"
```

---

## Task 5: `handleSnapshot` + route

**Files:**
- Modify: `internal/federation/coordinator/pins.go`
- Modify: `internal/federation/coordinator/server.go`
- Test: `internal/federation/coordinator/snapshot_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSnapshotPagingAndEpoch(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	for _, c := range []string{"bafa", "bafb", "bafc"} {
		seedBlob(t, ctx, pool, c, 1)
		assignViaSeam(t, ctx, pool, c, id)
	}

	// page 1 (limit 2) captures epoch, returns cursor
	w := httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet, "/fed/v1/pins/snapshot?limit=2", nil, leaf))
	var p1 wire.SnapshotResponse
	json.Unmarshal(w.Body.Bytes(), &p1)
	if w.Code != 200 || len(p1.Data) != 2 || p1.Cursor == "" || p1.SnapshotEpoch == 0 {
		t.Fatalf("page1: code=%d %+v", w.Code, p1)
	}

	// page 2 with epoch + cursor returns the rest, empty cursor
	w = httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet,
		"/fed/v1/pins/snapshot?limit=2&cursor="+p1.Cursor+"&snapshot_epoch="+itoa(p1.SnapshotEpoch), nil, leaf))
	var p2 wire.SnapshotResponse
	json.Unmarshal(w.Body.Bytes(), &p2)
	if len(p2.Data) != 1 || p2.Cursor != "" {
		t.Fatalf("page2: %+v", p2)
	}
}

func TestSnapshot409OnConcurrentSameNodeChange(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	for _, c := range []string{"bafa", "bafb"} {
		seedBlob(t, ctx, pool, c, 1)
		assignViaSeam(t, ctx, pool, c, id)
	}
	w := httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet, "/fed/v1/pins/snapshot?limit=1", nil, leaf))
	var p1 wire.SnapshotResponse
	json.Unmarshal(w.Body.Bytes(), &p1)

	// a new change for THIS node appears mid-pagination
	seedBlob(t, ctx, pool, "bafc", 1)
	assignViaSeam(t, ctx, pool, "bafc", id)

	w = httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet,
		"/fed/v1/pins/snapshot?limit=1&cursor="+p1.Cursor+"&snapshot_epoch="+itoa(p1.SnapshotEpoch), nil, leaf))
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/coordinator/ -run Snapshot -v`
Expected: FAIL — `handleSnapshot` undefined.

- [ ] **Step 3: Add `handleSnapshot` to `pins.go`**

```go
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	node, ok := s.authNode(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cursor := r.URL.Query().Get("cursor")
	limRaw, err := queryInt(r, "limit", defaultPinPageLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	limit := clampLimit(limRaw)

	head, err := s.q.GetChangeLogHead(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "head")
		return
	}
	epoch := head // first page captures the global head
	if ep := r.URL.Query().Get("snapshot_epoch"); ep != "" {
		epoch, err = queryInt(r, "snapshot_epoch", head)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		changed, err := s.q.NodeHasChangesAfter(ctx, gen.NodeHasChangesAfterParams{NodeID: pgUUIDFrom(node), Sequence: epoch})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "epoch-check")
			return
		}
		if changed {
			slog.Info("fed.snapshot.conflict", "node_id", node, "epoch", epoch)
			writeJSON(w, http.StatusConflict, map[string]any{"code": "snapshot_epoch_changed", "snapshot_epoch": head})
			return
		}
	}
	rows, err := s.q.GetPinSnapshotPage(ctx, gen.GetPinSnapshotPageParams{NodeID: pgUUIDFrom(node), Cid: cursor, Limit: int32(limit)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "snapshot")
		return
	}
	items := make([]wire.SnapshotItem, len(rows))
	for i, row := range rows {
		items[i] = wire.SnapshotItem{
			CID: row.Cid, AssignmentID: uuid.UUID(row.AssignmentID.Bytes).String(),
			Generation: row.Generation, ByteSize: row.ByteSize, AssignedAt: row.AssignedAt.Format(time.RFC3339),
		}
	}
	nextCursor := ""
	if int64(len(rows)) == int64(limit) && len(rows) > 0 {
		nextCursor = rows[len(rows)-1].Cid
	}
	slog.Info("fed.snapshot.page", "node_id", node, "epoch", epoch, "returned", len(rows))
	writeJSON(w, http.StatusOK, wire.SnapshotResponse{Data: items, Cursor: nextCursor, SnapshotEpoch: epoch})
}
```

Add `"time"` to imports. Register the route in `mux()`:

```go
m.HandleFunc("GET /fed/v1/pins/snapshot", s.handleSnapshot)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/coordinator/ -run Snapshot -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/federation/coordinator/pins.go internal/federation/coordinator/server.go internal/federation/coordinator/snapshot_test.go
git commit -m "feat(p2-m3): GET /fed/v1/pins/snapshot handler with epoch consistency (P2-M3)"
```

---

## Task 6: `handleAck` / `handleFail` + routes

**Files:**
- Modify: `internal/federation/coordinator/pins.go`
- Modify: `internal/federation/coordinator/server.go`
- Test: `internal/federation/coordinator/ack_test.go`

- [ ] **Step 1: Write the failing test**

```go
func ackBody(a wire.Ack) []byte { b, _ := json.Marshal(a); return b }

func TestAckSuccessStaleIdempotentUnknown(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	seedBlob(t, ctx, pool, "bafy1", 5)
	assignViaSeam(t, ctx, pool, "bafy1", id)

	cur, _ := s.q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{Cid: "bafy1", NodeID: pgUUIDFrom(id)})
	aid := uuid.UUID(cur.AssignmentID.Bytes).String()

	// success ⇒ 204
	w := httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/bafy1/ack",
		ackBody(wire.Ack{AssignmentID: aid, Generation: cur.Generation, CID: "bafy1"}), leaf))
	if w.Code != http.StatusNoContent {
		t.Fatalf("ack = %d, want 204 (%s)", w.Code, w.Body)
	}

	// idempotent re-ack same generation ⇒ 204
	w = httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/bafy1/ack",
		ackBody(wire.Ack{AssignmentID: aid, Generation: cur.Generation, CID: "bafy1"}), leaf))
	if w.Code != http.StatusNoContent {
		t.Fatalf("idempotent re-ack = %d, want 204", w.Code)
	}

	// stale (older generation) ⇒ 409
	w = httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/bafy1/ack",
		ackBody(wire.Ack{AssignmentID: aid, Generation: cur.Generation - 1, CID: "bafy1"}), leaf))
	if w.Code != http.StatusConflict {
		t.Fatalf("stale ack = %d, want 409", w.Code)
	}

	// unknown cid ⇒ 404
	w = httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/nope/ack",
		ackBody(wire.Ack{AssignmentID: aid, Generation: 1, CID: "nope"}), leaf))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown ack = %d, want 404", w.Code)
	}
}
```

> The handler reads `r.PathValue("cid")`, which is only populated when the request is matched against the `{cid}` route pattern — so the test routes through `s.mux().ServeHTTP(w, req)` (NOT a direct `s.handleAck` call, which would leave `cid==""` and trip the new `cid_mismatch` guard). `reqWithCert` still sets `r.TLS`, so auth works through the mux.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/coordinator/ -run Ack -v`
Expected: FAIL — `handleAck`/`handleFail` undefined.

- [ ] **Step 3: Add `handleAck` + `handleFail` to `pins.go`**

```go
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	node, ok := s.authNode(w, r)
	if !ok {
		return
	}
	cid := r.PathValue("cid")
	var req wire.Ack
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "malformed ack")
		return
	}
	if req.CID != "" && req.CID != cid {
		writeError(w, http.StatusBadRequest, "cid_mismatch", "body cid does not match path")
		return
	}
	aid, err := uuid.Parse(req.AssignmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_assignment_id", "")
		return
	}
	ctx := r.Context()
	n, err := s.q.AckPinAssignment(ctx, gen.AckPinAssignmentParams{
		Cid: cid, NodeID: pgUUIDFrom(node), AssignmentID: pgUUIDFrom(aid), Generation: req.Generation,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "ack")
		return
	}
	if n == 1 {
		slog.Info("fed.ack.applied", "cid", cid, "assignment_id", req.AssignmentID, "generation", req.Generation)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// 0 rows: idempotent replay vs stale vs unknown
	cur, err := s.q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{Cid: cid, NodeID: pgUUIDFrom(node)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "unknown_assignment", "")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "ack-lookup")
		return
	}
	if uuid.UUID(cur.AssignmentID.Bytes) == aid && cur.Generation == req.Generation && cur.State == gen.PinStateAcked {
		w.WriteHeader(http.StatusNoContent) // idempotent
		return
	}
	slog.Info("fed.ack.stale", "cid", cid, "assignment_id", req.AssignmentID, "generation", req.Generation)
	writeError(w, http.StatusConflict, wire.CodeStaleAssignment, "")
}

func (s *Server) handleFail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	node, ok := s.authNode(w, r)
	if !ok {
		return
	}
	cid := r.PathValue("cid")
	var req wire.Fail
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "malformed fail")
		return
	}
	if req.CID != "" && req.CID != cid {
		writeError(w, http.StatusBadRequest, "cid_mismatch", "body cid does not match path")
		return
	}
	if req.Reason = wire.NormalizeFailReason(req.Reason); req.Reason == "" {
		writeError(w, http.StatusBadRequest, "bad_reason", "unknown fail reason")
		return
	}
	aid, err := uuid.Parse(req.AssignmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_assignment_id", "")
		return
	}
	ctx := r.Context()
	n, err := s.q.FailPinAssignment(ctx, gen.FailPinAssignmentParams{
		Cid: cid, NodeID: pgUUIDFrom(node), AssignmentID: pgUUIDFrom(aid), Generation: req.Generation,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "fail")
		return
	}
	if n == 1 {
		slog.Info("fed.fail.applied", "cid", cid, "reason", req.Reason, "generation", req.Generation)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cur, err := s.q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{Cid: cid, NodeID: pgUUIDFrom(node)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "unknown_assignment", "")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "fail-lookup")
		return
	}
	if uuid.UUID(cur.AssignmentID.Bytes) == aid && cur.Generation == req.Generation && cur.State == gen.PinStateFailed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeError(w, http.StatusConflict, wire.CodeStaleAssignment, "")
}
```

Register the routes in `mux()`:

```go
m.HandleFunc("POST /fed/v1/pins/{cid}/ack", s.handleAck)
m.HandleFunc("POST /fed/v1/pins/{cid}/fail", s.handleFail)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/coordinator/ -run Ack -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/federation/coordinator/pins.go internal/federation/coordinator/server.go internal/federation/coordinator/ack_test.go
git commit -m "feat(p2-m3): ack/fail handlers with generation-keyed state machine (P2-M3)"
```

---

## Task 7: Retention / prune + heartbeat epoch + config

**Files:**
- Create: `internal/federation/coordinator/retention.go`
- Modify: `internal/federation/coordinator/server.go` (Config + Run), `handlers.go` (heartbeat epoch)
- Modify: `internal/config/*` + `cmd/coordinator/main.go` (defaults)
- Test: `internal/federation/coordinator/retention_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestPruneAdvancesWatermarkAndTriggersSnapshot(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	seedBlob(t, ctx, pool, "bafy1", 1)
	assignViaSeam(t, ctx, pool, "bafy1", id)
	// backdate the change so it is older than the retention cutoff
	pool.Exec(ctx, `UPDATE pin_changes SET created_at = now() - interval '30 days'`)

	if err := s.pruneOnce(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	var remaining int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_changes`).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("pin_changes remaining = %d, want 0", remaining)
	}
	// a poll below the new watermark now demands snapshot recovery
	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	var er wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &er)
	if w.Code != http.StatusBadRequest || er.Code != wire.CodeSnapshotRequired {
		t.Fatalf("post-prune poll: code=%d %q", w.Code, er.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/coordinator/ -run Prune -v`
Expected: FAIL — `pruneOnce` undefined.

- [ ] **Step 3: Create `internal/federation/coordinator/retention.go`**

```go
package coordinator

import (
	"context"
	"log/slog"
	"time"
)

// pruneOnce deletes change-log rows older than retention and advances the
// watermark (one atomic statement). A donor whose cursor predates the watermark
// then recovers via snapshot. NB: sqlc maps timestamptz→time.Time (see
// internal/db/sqlc.yaml override), so PruneChangeLog takes a time.Time directly.
func (s *Server) pruneOnce(ctx context.Context, retention time.Duration) error {
	wm, err := s.q.PruneChangeLog(ctx, time.Now().Add(-retention))
	if err != nil {
		return err
	}
	slog.Info("fed.changelog.pruned", "pruned_through_seq", wm)
	return nil
}

// runRetention prunes on a ticker until ctx is cancelled. Started from Run.
func (s *Server) runRetention(ctx context.Context, interval, retention time.Duration) {
	if interval <= 0 || retention <= 0 {
		return // retention disabled
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.pruneOnce(ctx, retention); err != nil {
				slog.Warn("fed.changelog.prune_failed", "err", err)
			}
		}
	}
}
```

- [ ] **Step 4: Wire Config + Run + heartbeat epoch**

In `server.go`, extend `Config`:

```go
type Config struct {
	ListenAddr           string
	RequiredCapabilities []string
	Timers               wire.ConfigUpdates
	TLS                  TLSMaterial
	ChangeLogRetention   time.Duration // default 168h; 0 disables pruning
	PrunePollInterval    time.Duration // default 1h
}
```

In `Run`, start the goroutine before serving:

```go
go s.runRetention(ctx, s.cfg.PrunePollInterval, s.cfg.ChangeLogRetention)
```

In `handlers.go`, replace the hard-coded epoch in `handleHeartbeat`:

```go
head, err := s.q.GetChangeLogHead(ctx)
if err != nil {
	writeError(w, http.StatusInternalServerError, "internal", "change-log head")
	return
}
timers := s.cfg.Timers
writeJSON(w, http.StatusOK, wire.HeartbeatResponse{
	ConfigUpdates:        &timers,
	CurrentEpoch:         head,
	RepairTokenPublicKey: "", // M4
})
```

In `cmd/coordinator/main.go`, default the two new fields when building `coordinator.Config` (retention `168h`, prune interval `1h`; surface them under the existing `federation` config block in `internal/config` for operator tuning).

- [ ] **Step 5: Run tests + commit**

Run: `go test ./internal/federation/coordinator/ -run 'Prune|Heartbeat' -v && go build ./cmd/coordinator/`
Expected: PASS / build OK.

```bash
git add internal/federation/coordinator/retention.go internal/federation/coordinator/server.go internal/federation/coordinator/handlers.go internal/federation/coordinator/retention_test.go internal/config/ cmd/coordinator/main.go
git commit -m "feat(p2-m3): change-log retention/prune + real heartbeat epoch (P2-M3)"
```

---

## Task 8: Donor `FileStore` (durable cursor)

**Files:**
- Create: `internal/node/state/store_file.go`
- Test: `internal/node/state/store_file_test.go`

- [ ] **Step 1: Write the failing test**

```go
package state

import (
	"path/filepath"
	"testing"
)

func TestFileStoreCursorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	if c, err := s.Cursor(); err != nil || c != 0 {
		t.Fatalf("empty cursor: c=%d err=%v", c, err)
	}
	if err := s.SetCursor(4172836); err != nil {
		t.Fatal(err)
	}
	// reopen ⇒ survives
	if c, err := NewFileStore(dir).Cursor(); err != nil || c != 4172836 {
		t.Fatalf("reopened cursor: c=%d err=%v", c, err)
	}
	if _, err := filepath.Glob(filepath.Join(dir, "state", "*.tmp")); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreImplementsStore(t *testing.T) {
	var _ Store = NewFileStore(t.TempDir())
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/state/ -run FileStore -v`
Expected: FAIL — `NewFileStore` undefined.

- [ ] **Step 3: Create `internal/node/state/store_file.go`** (reuse `fsyncDir` from `registration.go`)

```go
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileStore is the durable donor Store: the change-log cursor persists to
// <storageDir>/state/cursor.json (atomic temp→rename). The jti replay cache is
// in-memory for M3 (single-use repair tokens are M4).
type FileStore struct {
	dir  string
	mu   sync.Mutex
	jtis map[string]time.Time
}

func NewFileStore(storageDir string) *FileStore {
	return &FileStore{dir: filepath.Join(storageDir, "state"), jtis: map[string]time.Time{}}
}

func (f *FileStore) path() string { return filepath.Join(f.dir, "cursor.json") }

type cursorDoc struct {
	Seq int64 `json:"seq"`
}

func (f *FileStore) Cursor() (int64, error) {
	data, err := os.ReadFile(f.path())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var d cursorDoc
	if err := json.Unmarshal(data, &d); err != nil {
		return 0, err
	}
	return d.Seq, nil
}

func (f *FileStore) SetCursor(seq int64) error {
	data, _ := json.MarshalIndent(cursorDoc{Seq: seq}, "", "  ")
	return atomicWrite(f.dir, "cursor", data)
}

func (f *FileStore) SeenJTI(jti string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	exp, ok := f.jtis[jti]
	if !ok {
		return false, nil
	}
	if !time.Now().Before(exp) {
		delete(f.jtis, jti)
		return false, nil
	}
	return true, nil
}

func (f *FileStore) RecordJTI(jti string, exp time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jtis[jti] = exp
	return nil
}

var _ Store = (*FileStore)(nil)
```

Extract the atomic-write body of `FileRegistrationStore.SaveRegistration` into a shared helper in `registration.go` (so both stores reuse it):

```go
// atomicWrite writes <dir>/<name>.json atomically (temp→fsync→rename→dir fsync),
// 0600 file / 0700 dir.
func atomicWrite(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+"-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name+".json")); err != nil {
		return err
	}
	return fsyncDir(dir)
}
```

Then refactor `SaveRegistration` to call `atomicWrite(f.dir, "registration", data)` (keeps the M2 round-trip test green).

- [ ] **Step 4: Run tests + commit**

Run: `go test ./internal/node/state/ -v`
Expected: PASS (M2 registration test still green).

```bash
git add internal/node/state/store_file.go internal/node/state/store_file_test.go internal/node/state/registration.go
git commit -m "feat(p2-m3): donor FileStore durable cursor + shared atomicWrite (P2-M3)"
```

---

## Task 9: Donor `FileAssignmentStore` (desired set)

**Files:**
- Create: `internal/node/state/assignments.go`
- Test: `internal/node/state/assignments_test.go`

- [ ] **Step 1: Write the failing test**

```go
package state

import "testing"

func TestAssignmentStoreApplyIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := NewFileAssignmentStore(dir)

	// assign gen 1
	if err := s.ApplyChanges([]ChangeInput{{AssignmentID: "a", Generation: 1, Kind: "assign", CID: "bafy1", ByteSize: 5}}); err != nil {
		t.Fatal(err)
	}
	// replay same change ⇒ no-op
	s.ApplyChanges([]ChangeInput{{AssignmentID: "a", Generation: 1, Kind: "assign", CID: "bafy1", ByteSize: 5}})
	// bump to gen 2
	s.ApplyChanges([]ChangeInput{{AssignmentID: "a", Generation: 2, Kind: "assign", CID: "bafy1", ByteSize: 5}})

	got, _ := NewFileAssignmentStore(dir).List() // reopened ⇒ durable
	if len(got) != 1 || got[0].Generation != 2 || got[0].State != "pending" {
		t.Fatalf("after assigns: %+v", got)
	}

	// stale generation is ignored
	s = NewFileAssignmentStore(dir)
	s.ApplyChanges([]ChangeInput{{AssignmentID: "a", Generation: 1, Kind: "unpin", CID: "bafy1"}})
	if got, _ := s.List(); len(got) != 1 {
		t.Fatalf("stale unpin must be ignored: %+v", got)
	}
	// current-generation unpin removes
	s.ApplyChanges([]ChangeInput{{AssignmentID: "a", Generation: 3, Kind: "unpin", CID: "bafy1"}})
	if got, _ := s.List(); len(got) != 0 {
		t.Fatalf("unpin should remove: %+v", got)
	}
}

func TestAssignmentStoreReplaceFromSnapshot(t *testing.T) {
	dir := t.TempDir()
	s := NewFileAssignmentStore(dir)
	s.ApplyChanges([]ChangeInput{{AssignmentID: "old", Generation: 1, Kind: "assign", CID: "stale", ByteSize: 1}})
	if err := s.Replace([]DesiredAssignment{{CID: "bafy1", AssignmentID: "a", Generation: 7, ByteSize: 5, State: "pending"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := NewFileAssignmentStore(dir).List()
	if len(got) != 1 || got[0].CID != "bafy1" || got[0].Generation != 7 {
		t.Fatalf("replace should wholesale-replace: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/state/ -run Assignment -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Create `internal/node/state/assignments.go`**

```go
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// DesiredAssignment is one entry of the donor's local desired-assignment set —
// coordinator INTENT this node hold a CID. It is NOT evidence the bytes are held
// (that is an ack, M4). State is "pending" until M4's fetch→verify→ack.
type DesiredAssignment struct {
	CID          string `json:"cid"`
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	ByteSize     int64  `json:"byte_size"`
	State        string `json:"state"` // "pending" in M3
}

// ChangeInput is a primitive view of a wire.PinChange (state stays wire-free).
type ChangeInput struct {
	AssignmentID string
	Generation   int64
	Kind         string // "assign" | "unpin"
	CID          string
	ByteSize     int64
}

// AssignmentStore is the donor's durable desired-assignment set.
type AssignmentStore interface {
	ApplyChanges(changes []ChangeInput) error // idempotent by (assignment_id, generation)
	Replace(items []DesiredAssignment) error  // wholesale snapshot replace
	List() ([]DesiredAssignment, error)
}

// FileAssignmentStore persists the set to <storageDir>/state/assignments.json.
type FileAssignmentStore struct {
	dir string
	mu  sync.Mutex
}

func NewFileAssignmentStore(storageDir string) *FileAssignmentStore {
	return &FileAssignmentStore{dir: filepath.Join(storageDir, "state")}
}

func (f *FileAssignmentStore) path() string { return filepath.Join(f.dir, "assignments.json") }

func (f *FileAssignmentStore) load() (map[string]DesiredAssignment, error) {
	data, err := os.ReadFile(f.path())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]DesiredAssignment{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]DesiredAssignment
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *FileAssignmentStore) persist(m map[string]DesiredAssignment) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	return atomicWrite(f.dir, "assignments", data)
}

func (f *FileAssignmentStore) ApplyChanges(changes []ChangeInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	for _, c := range changes {
		cur, ok := m[c.CID]
		// idempotent: only act on a generation >= what we have for this assignment.
		if ok && cur.AssignmentID == c.AssignmentID && c.Generation < cur.Generation {
			continue
		}
		switch c.Kind {
		case "assign":
			m[c.CID] = DesiredAssignment{CID: c.CID, AssignmentID: c.AssignmentID, Generation: c.Generation, ByteSize: c.ByteSize, State: "pending"}
		case "unpin":
			delete(m, c.CID)
		}
	}
	return f.persist(m)
}

func (f *FileAssignmentStore) Replace(items []DesiredAssignment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := make(map[string]DesiredAssignment, len(items))
	for _, it := range items {
		if it.State == "" {
			it.State = "pending"
		}
		m[it.CID] = it
	}
	return f.persist(m)
}

func (f *FileAssignmentStore) List() ([]DesiredAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return nil, err
	}
	out := make([]DesiredAssignment, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CID < out[j].CID })
	return out, nil
}

var _ AssignmentStore = (*FileAssignmentStore)(nil)
```

- [ ] **Step 4: Run tests + commit**

Run: `go test ./internal/node/state/ -v`
Expected: PASS.

```bash
git add internal/node/state/assignments.go internal/node/state/assignments_test.go
git commit -m "feat(p2-m3): donor durable desired-assignment set (P2-M3)"
```

---

## Task 10: Donor client `GetChanges` / `GetSnapshot`

**Files:**
- Modify: `internal/node/agent/client.go`
- Test: `internal/node/agent/client_test.go` (extend; httptest server)

- [ ] **Step 1: Write the failing test**

```go
func TestGetChangesAndSnapshotRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fed/v1/pins/changes":
			if r.URL.Query().Get("since_seq") == "999" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(wire.ErrorResponse{Code: wire.CodeSnapshotRequired})
				return
			}
			json.NewEncoder(w).Encode(wire.ChangesResponse{Changes: []wire.PinChange{{Sequence: 1, Kind: "assign", CID: "bafy1"}}, NextSeq: 1, CurrentEpoch: 1})
		case "/fed/v1/pins/snapshot":
			json.NewEncoder(w).Encode(wire.SnapshotResponse{Data: []wire.SnapshotItem{{CID: "bafy1", AssignmentID: "a", Generation: 1}}, Cursor: "", SnapshotEpoch: 5})
		}
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, nil)

	resp, err := c.GetChanges(context.Background(), 0)
	if err != nil || len(resp.Changes) != 1 {
		t.Fatalf("GetChanges: %+v err=%v", resp, err)
	}
	if _, err := c.GetChanges(context.Background(), 999); !errors.Is(err, ErrSnapshotRequired) {
		t.Fatalf("want ErrSnapshotRequired, got %v", err)
	}
	snap, err := c.GetSnapshot(context.Background(), "", 0)
	if err != nil || snap.SnapshotEpoch != 5 {
		t.Fatalf("GetSnapshot: %+v err=%v", snap, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/agent/ -run GetChanges -v`
Expected: FAIL — methods/sentinels undefined.

- [ ] **Step 3: Add to `internal/node/agent/client.go`**

```go
// Sentinels the agent branches on.
var (
	ErrSnapshotRequired     = errors.New("agent: snapshot_required")
	ErrSnapshotEpochChanged = errors.New("agent: snapshot epoch changed")
)

func (c *HTTPClient) GetChanges(ctx context.Context, sinceSeq int64) (wire.ChangesResponse, error) {
	u := fmt.Sprintf("%s/fed/v1/pins/changes?since_seq=%d&limit=%d", c.base, sinceSeq, 1000)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return wire.ChangesResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		var er wire.ErrorResponse
		json.NewDecoder(resp.Body).Decode(&er)
		if er.Code == wire.CodeSnapshotRequired {
			return wire.ChangesResponse{}, ErrSnapshotRequired
		}
		return wire.ChangesResponse{}, fmt.Errorf("changes: %s", er.Code)
	}
	if resp.StatusCode != http.StatusOK {
		return wire.ChangesResponse{}, fmt.Errorf("changes: status %d", resp.StatusCode)
	}
	var out wire.ChangesResponse
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func (c *HTTPClient) GetSnapshot(ctx context.Context, cursor string, epoch int64) (wire.SnapshotResponse, error) {
	u := fmt.Sprintf("%s/fed/v1/pins/snapshot?limit=%d", c.base, 1000)
	if cursor != "" {
		u += "&cursor=" + url.QueryEscape(cursor)
	}
	if epoch > 0 {
		u += "&snapshot_epoch=" + strconv.FormatInt(epoch, 10)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return wire.SnapshotResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return wire.SnapshotResponse{}, ErrSnapshotEpochChanged
	}
	if resp.StatusCode != http.StatusOK {
		return wire.SnapshotResponse{}, fmt.Errorf("snapshot: status %d", resp.StatusCode)
	}
	var out wire.SnapshotResponse
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
```

Add imports `errors`, `fmt`, `net/url`, `strconv` as needed.

- [ ] **Step 4: Run tests + commit**

Run: `go test ./internal/node/agent/ -run GetChanges -v`
Expected: PASS.

```bash
git add internal/node/agent/client.go internal/node/agent/client_test.go
git commit -m "feat(p2-m3): donor client GetChanges/GetSnapshot + sentinels (P2-M3)"
```

---

## Task 11: Donor agent — pins-poll loop (no ack)

**Files:**
- Modify: `internal/node/agent/agent.go`
- Test: `internal/node/agent/agent_sync_test.go`

- [ ] **Step 1: Write the failing test**

```go
package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/state"
)

type syncFake struct {
	acks       atomic.Int32 // MUST stay 0 in M3
	changes    []wire.ChangesResponse
	idx        atomic.Int32
	snapResp   wire.SnapshotResponse
	forceSnap  bool
}

func (f *syncFake) Register(context.Context, wire.RegisterRequest) (wire.RegisterResponse, error) {
	return wire.RegisterResponse{NodeID: "n1", SelectedProtocol: wire.ProtocolV1}, nil
}
func (f *syncFake) Heartbeat(context.Context, wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	return wire.HeartbeatResponse{ConfigUpdates: &wire.ConfigUpdates{}}, nil
}
func (f *syncFake) GetChanges(_ context.Context, since int64) (wire.ChangesResponse, error) {
	if f.forceSnap && since == 0 {
		return wire.ChangesResponse{}, ErrSnapshotRequired
	}
	i := int(f.idx.Add(1)) - 1
	if i < len(f.changes) {
		return f.changes[i], nil
	}
	return wire.ChangesResponse{NextSeq: since, CurrentEpoch: since}, nil
}
func (f *syncFake) GetSnapshot(context.Context, string, int64) (wire.SnapshotResponse, error) {
	return f.snapResp, nil
}

func TestSyncOnceAppliesIdempotentlyAndNeverAcks(t *testing.T) {
	dir := t.TempDir()
	cur := state.NewFileStore(dir)
	asg := state.NewFileAssignmentStore(dir)
	f := &syncFake{changes: []wire.ChangesResponse{
		{Changes: []wire.PinChange{{Sequence: 1, AssignmentID: "a", Generation: 1, Kind: "assign", CID: "bafy1", ByteSize: 5}}, NextSeq: 1, CurrentEpoch: 1},
	}}
	a := New(&config.Config{}, state.NewFileRegistrationStore(dir), cur, asg, f, time.Second, time.Second)

	next := a.syncOnce(context.Background(), 0)
	if next != 1 {
		t.Fatalf("cursor = %d, want 1", next)
	}
	got, _ := asg.List()
	if len(got) != 1 || got[0].State != "pending" {
		t.Fatalf("desired set: %+v", got)
	}
	if f.acks.Load() != 0 {
		t.Fatal("donor must NOT ack in M3")
	}
	// replay (same change) is a no-op
	f.idx.Store(0)
	a.syncOnce(context.Background(), 0)
	if got, _ := asg.List(); len(got) != 1 {
		t.Fatalf("replay not idempotent: %+v", got)
	}
}

func TestSyncSnapshotRecovery(t *testing.T) {
	dir := t.TempDir()
	cur := state.NewFileStore(dir)
	asg := state.NewFileAssignmentStore(dir)
	f := &syncFake{forceSnap: true, snapResp: wire.SnapshotResponse{
		Data: []wire.SnapshotItem{{CID: "bafy1", AssignmentID: "a", Generation: 3, ByteSize: 5}}, Cursor: "", SnapshotEpoch: 9,
	}}
	a := New(&config.Config{}, state.NewFileRegistrationStore(dir), cur, asg, f, time.Second, time.Second)

	next := a.syncOnce(context.Background(), 0)
	if next != 9 {
		t.Fatalf("cursor after recovery = %d, want 9", next)
	}
	got, _ := asg.List()
	if len(got) != 1 || got[0].Generation != 3 {
		t.Fatalf("recovered set: %+v", got)
	}
}

func TestSyncUnknownKindFailsClosed(t *testing.T) {
	dir := t.TempDir()
	asg := state.NewFileAssignmentStore(dir)
	f := &syncFake{
		changes:  []wire.ChangesResponse{{Changes: []wire.PinChange{{Sequence: 1, Kind: "frobnicate", CID: "x"}}, NextSeq: 1}},
		snapResp: wire.SnapshotResponse{Data: nil, Cursor: "", SnapshotEpoch: 2},
	}
	a := New(&config.Config{}, state.NewFileRegistrationStore(t.TempDir()), state.NewFileStore(dir), asg, f, time.Second, time.Second)
	a.syncOnce(context.Background(), 0)
	// unknown kind ⇒ fail closed ⇒ snapshot recovery wipes to the snapshot (empty)
	if got, _ := asg.List(); len(got) != 0 {
		t.Fatalf("unknown kind must not apply: %+v", got)
	}
	if f.acks.Load() != 0 {
		t.Fatal("no ack on fail-closed")
	}
}
```

(Imports: `time`, `nodeconfig "…/internal/node/config"` aliased as `config`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/agent/ -run Sync -v`
Expected: FAIL — new `New` signature + `syncOnce` undefined.

- [ ] **Step 3: Rework `internal/node/agent/agent.go`**

Extend the `Client` interface and the `Agent`/`New`; add `syncOnce`/`recoverSnapshot`; add the pins-poll ticker to `Run`. The donor advertises the M3 capabilities and **never** calls ack/fail.

```go
type Client interface {
	Register(ctx context.Context, req wire.RegisterRequest) (wire.RegisterResponse, error)
	Heartbeat(ctx context.Context, req wire.HeartbeatRequest) (wire.HeartbeatResponse, error)
	GetChanges(ctx context.Context, sinceSeq int64) (wire.ChangesResponse, error)
	GetSnapshot(ctx context.Context, cursor string, epoch int64) (wire.SnapshotResponse, error)
}

type Agent struct {
	cfg          *nodeconfig.Config
	reg          state.RegistrationStore
	cursor       state.Store
	assignments  state.AssignmentStore
	client       Client
	hbInterval   time.Duration
	pollInterval time.Duration
}

func New(cfg *nodeconfig.Config, reg state.RegistrationStore, cursor state.Store, asg state.AssignmentStore, client Client, hb, poll time.Duration) *Agent {
	return &Agent{cfg: cfg, reg: reg, cursor: cursor, assignments: asg, client: client, hbInterval: hb, pollInterval: poll}
}

func (a *Agent) registerReq() wire.RegisterRequest {
	return wire.RegisterRequest{
		SupportedProtocols:         []string{wire.ProtocolV1},
		Capabilities:               []string{wire.CapPinChangeLog, wire.CapSnapshot},
		BandwidthBudgetBytesPerDay: a.cfg.BandwidthBudgetBytesPerDay,
	}
}

// syncOnce pulls one batch of changes (or recovers via snapshot) and returns the
// new cursor. It applies changes idempotently and NEVER acks (M4 owns ack).
func (a *Agent) syncOnce(ctx context.Context, cursor int64) int64 {
	resp, err := a.client.GetChanges(ctx, cursor)
	if errors.Is(err, ErrSnapshotRequired) {
		return a.recoverSnapshot(ctx, cursor)
	}
	if err != nil {
		slog.Warn("node.sync.poll_failed", "err", err)
		return cursor
	}
	inputs := make([]state.ChangeInput, 0, len(resp.Changes))
	for _, ch := range resp.Changes {
		if ch.Kind != wire.ChangeKindAssign && ch.Kind != wire.ChangeKindUnpin {
			slog.Error("node.sync.failclosed", "kind", ch.Kind, "seq", ch.Sequence)
			return a.recoverSnapshot(ctx, cursor) // fail closed: stop, re-sync from snapshot
		}
		inputs = append(inputs, state.ChangeInput{
			AssignmentID: ch.AssignmentID, Generation: ch.Generation, Kind: ch.Kind, CID: ch.CID, ByteSize: ch.ByteSize,
		})
	}
	if err := a.assignments.ApplyChanges(inputs); err != nil {
		slog.Warn("node.state.write_error", "err", err)
		return cursor // do not advance the cursor past unpersisted state
	}
	if err := a.cursor.SetCursor(resp.NextSeq); err != nil {
		slog.Warn("node.state.write_error", "err", err)
		return cursor
	}
	slog.Info("node.sync.applied", "from_seq", cursor, "to_seq", resp.NextSeq, "applied", len(inputs))
	return resp.NextSeq
}

// recoverSnapshot rebuilds the desired set from a full snapshot and returns the
// new cursor (= snapshot_epoch) ONLY after both the set and the cursor persist;
// on any error it returns oldCursor unchanged, so neither in-memory nor durable
// cursor state ever skips unpersisted assignments ("set first, cursor second").
func (a *Agent) recoverSnapshot(ctx context.Context, oldCursor int64) int64 {
	var all []state.DesiredAssignment
	cursor := ""
	var epoch int64
	for {
		resp, err := a.client.GetSnapshot(ctx, cursor, epoch)
		if errors.Is(err, ErrSnapshotEpochChanged) {
			all, cursor, epoch = nil, "", 0 // restart from page 1
			continue
		}
		if err != nil {
			slog.Warn("node.sync.snapshot_failed", "err", err)
			return oldCursor
		}
		epoch = resp.SnapshotEpoch
		for _, it := range resp.Data {
			all = append(all, state.DesiredAssignment{CID: it.CID, AssignmentID: it.AssignmentID, Generation: it.Generation, ByteSize: it.ByteSize, State: "pending"})
		}
		if resp.Cursor == "" {
			break
		}
		cursor = resp.Cursor
	}
	if err := a.assignments.Replace(all); err != nil {
		slog.Warn("node.state.write_error", "err", err)
		return oldCursor
	}
	if err := a.cursor.SetCursor(epoch); err != nil {
		slog.Warn("node.state.write_error", "err", err)
		return oldCursor
	}
	slog.Info("node.sync.recovered", "snapshot_epoch", epoch, "items", len(all))
	return epoch
}
```

In `Run`, after the register block, load the cursor and add the second ticker:

```go
cursor, _ := a.cursor.Cursor()
cursor = a.syncOnce(ctx, cursor) // immediate first sync: learn assignments / catch snapshot_required without waiting a full poll interval
hb := time.NewTicker(a.hbInterval)
defer hb.Stop()
poll := time.NewTicker(a.pollInterval)
defer poll.Stop()
for {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-hb.C:
		resp, err := a.client.Heartbeat(ctx, wire.HeartbeatRequest{})
		if err != nil {
			slog.Warn("nova-node heartbeat failed", "err", err)
			continue
		}
		if u := resp.ConfigUpdates; u != nil {
			if u.HeartbeatIntervalSeconds > 0 {
				if d := time.Duration(u.HeartbeatIntervalSeconds) * time.Second; d != a.hbInterval {
					a.hbInterval = d
					hb.Reset(d)
				}
			}
			if u.PinsPollIntervalSeconds > 0 {
				if d := time.Duration(u.PinsPollIntervalSeconds) * time.Second; d != a.pollInterval {
					a.pollInterval = d
					poll.Reset(d)
				}
			}
		}
	case <-poll.C:
		cursor = a.syncOnce(ctx, cursor)
	}
}
```

(`state.RegistrationStore` is now field `a.reg`; update the register block accordingly.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/node/agent/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/node/agent/agent.go internal/node/agent/agent_sync_test.go
git commit -m "feat(p2-m3): donor pins-poll sync loop (apply/recover/fail-closed, no ack) (P2-M3)"
```

---

## Task 12: Wire the donor stores into `cmd/node`

**Files:**
- Modify: `cmd/node/main.go`

- [ ] **Step 1: Update the agent construction**

Construct the durable stores and pass the poll interval (default 600s; honor `config_updates` thereafter):

```go
regStore := state.NewFileRegistrationStore(cfg.StorageDir)
cursor := state.NewFileStore(cfg.StorageDir)
assignments := state.NewFileAssignmentStore(cfg.StorageDir)
ag := agent.New(cfg, regStore, cursor, assignments, client,
	300*time.Second, 600*time.Second)
```

(Adjust the existing `agent.New(...)` call site to the new signature; the M2 wiring passed only `regStore`.)

- [ ] **Step 2: Verify build**

Run: `go build ./cmd/node/ && ./scripts/check_node_deps.sh`
Expected: build OK; donor dependency boundary still green (no new operator-only imports — `state` + `wire` are already donor-safe).

- [ ] **Step 3: Commit**

```bash
git add cmd/node/main.go
git commit -m "feat(p2-m3): wire donor cursor/assignment stores + pins poll (P2-M3)"
```

---

## Task 13: `novactl pin assign|unpin|list`

**Files:**
- Create: `cmd/novactl/pin.go`
- Modify: `cmd/novactl/node_db.go` (add `withNodeDBPool`)
- Modify: `cmd/novactl/main.go` (dispatch + usage)
- Test: `cmd/novactl/pin_db_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestPinAssignListUnpin(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	// seed node + blob
	node := uuid.New()
	pool.Exec(ctx, `INSERT INTO nodes (id,nebula_cert_fingerprint,federation_cert_fingerprint,capacity_bytes,bandwidth_budget_bytes_per_day) VALUES ($1,$2,$3,0,0)`,
		pgtype.UUID{Bytes: node, Valid: true}, "neb", "fed")
	pool.Exec(ctx, `INSERT INTO blobs (cid,mime_type,byte_size) VALUES ('bafy1','application/octet-stream',5)`)

	q := gen.New(pool)
	// assign via the same internal path the CLI uses
	tx, _ := pool.Begin(ctx)
	if _, err := coordinator.AssignPin(ctx, tx, "bafy1", node); err != nil {
		t.Fatal(err)
	}
	tx.Commit(ctx)

	desired, _ := q.ListDesiredAssignmentsByCID(ctx, "bafy1")
	verified, _ := q.ListVerifiedHoldersByCID(ctx, "bafy1")
	if len(desired) != 1 || len(verified) != 0 {
		t.Fatalf("desired=%d verified=%d (verified must be 0 in M3)", len(desired), len(verified))
	}

	// unpin removes the desired assignment
	tx, _ = pool.Begin(ctx)
	coordinator.UnpinPin(ctx, tx, "bafy1", node)
	tx.Commit(ctx)
	desired, _ = q.ListDesiredAssignmentsByCID(ctx, "bafy1")
	if len(desired) != 0 {
		t.Fatalf("after unpin desired = %d, want 0", len(desired))
	}
}
```

(This exercises the seam + list queries the CLI wraps; a thin `cmdPinList` formatting test can be added if desired.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/novactl/ -run Pin -v`
Expected: FAIL until the queries/seam are reachable (compile) — green once Tasks 2–3 are merged; this task adds the CLI surface.

- [ ] **Step 3: Add `withNodeDBPool` to `node_db.go`**

```go
func withNodeDBPool(fn func(ctx context.Context, pool *pgxpool.Pool) error) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL must be set for pin commands")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return fn(ctx, pool)
}
```

(Import `github.com/jackc/pgx/v5/pgxpool`.)

- [ ] **Step 4: Create `cmd/novactl/pin.go`**

```go
package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/coordinator"
)

func cmdPin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: novactl pin <assign|unpin|list> ...")
	}
	switch args[0] {
	case "assign":
		return cmdPinAssign(args[1:])
	case "unpin":
		return cmdPinUnpin(args[1:])
	case "list":
		return cmdPinList(args[1:])
	default:
		return fmt.Errorf("novactl pin: unknown subcommand %q", args[0])
	}
}

func pinMutate(args, name string, fn func(ctx context.Context, tx pgxTx, cid string, node uuid.UUID) (coordinator.Assignment, error)) error {
	fs := flag.NewFlagSet("pin "+name, flag.ContinueOnError)
	cid := fs.String("cid", "", "blob CID (required)")
	nodeStr := fs.String("node", "", "donor node UUID (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	node, err := uuid.Parse(*nodeStr)
	if err != nil {
		return fmt.Errorf("invalid --node: %w", err)
	}
	return withNodeDBPool(func(ctx context.Context, pool *pgxpool.Pool) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		a, err := fn(ctx, tx, *cid, node)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		fmt.Printf("%s cid=%s node=%s assignment_id=%s generation=%d sequence=%d\n", name, *cid, node, a.AssignmentID, a.Generation, a.Sequence)
		return nil
	})
}

func cmdPinAssign(args []string) error {
	return pinMutate(args, "assign", coordinator.AssignPin)
}
func cmdPinUnpin(args []string) error {
	return pinMutate(args, "unpin", coordinator.UnpinPin)
}

func cmdPinList(args []string) error {
	fs := flag.NewFlagSet("pin list", flag.ContinueOnError)
	cid := fs.String("cid", "", "list by blob CID")
	nodeStr := fs.String("node", "", "list by donor node UUID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		switch {
		case *cid != "":
			desired, err := q.ListDesiredAssignmentsByCID(ctx, *cid)
			if err != nil {
				return err
			}
			verified, err := q.ListVerifiedHoldersByCID(ctx, *cid)
			if err != nil {
				return err
			}
			fmt.Printf("desired assignments (%d):\n", len(desired))
			for _, d := range desired {
				fmt.Printf("  node=%s generation=%d state=%s\n", uuid.UUID(d.NodeID.Bytes), d.Generation, d.State)
			}
			fmt.Printf("verified holders (%d):\n", len(verified))
			for _, v := range verified {
				fmt.Printf("  node=%s generation=%d\n", uuid.UUID(v.NodeID.Bytes), v.Generation)
			}
		case *nodeStr != "":
			node, err := uuid.Parse(*nodeStr)
			if err != nil {
				return err
			}
			rows, err := q.ListDesiredAssignmentsByNode(ctx, pgtype.UUID{Bytes: node, Valid: true})
			if err != nil {
				return err
			}
			fmt.Printf("desired assignments for node %s (%d):\n", node, len(rows))
			for _, r := range rows {
				fmt.Printf("  cid=%s generation=%d state=%s\n", r.Cid, r.Generation, r.State)
			}
		default:
			return fmt.Errorf("pin list: pass --cid or --node")
		}
		return nil
	})
}
```

> `pgxTx` in the helper signature is `pgx.Tx`; import it (or inline the two calls without the generic helper — `coordinator.AssignPin`/`UnpinPin` already share the `(ctx, pgx.Tx, cid, uuid)` signature). Adjust generated field names (`d.NodeID`, `v.NodeID`, `r.Cid`) to the actual Task-2 output.

Dispatch in `cmd/novactl/main.go`:

```go
case "pin":
	err = cmdPin(args[1:])
```

…and add a `pin` line to `usage()`.

- [ ] **Step 5: Run tests + build + commit**

Run: `go test ./cmd/novactl/ -run Pin -v && go build ./cmd/novactl/`
Expected: PASS / build OK.

```bash
git add cmd/novactl/pin.go cmd/novactl/node_db.go cmd/novactl/main.go cmd/novactl/pin_db_test.go
git commit -m "feat(p2-m3): novactl pin assign|unpin|list (DB-direct) (P2-M3)"
```

---

## Task 14: End-to-end integration test (register → sync → recover)

**Files:**
- Modify: `internal/federation/coordinator/integration_test.go`

- [ ] **Step 1: Write the integration test**

Real `Server` + real `Agent`/`HTTPClient` over loopback mTLS + `dbtest`.

> **Correction (applied during execution):** `Agent.syncOnce` is **unexported** and this test is in package `coordinator`, so it cannot be called cross-package. The test instead drives the donor via the exported `ag.Run` (short poll interval) and polls the `FileAssignmentStore` to convergence with a deadline (`waitFor`). The snapshot-recovery path is forced deterministically by registering the node, assigning, pruning the change log (watermark advances to head), then starting a **fresh-cursor** donor whose first `GetChanges(0)` is below the watermark → `snapshot_required` → snapshot recovery. The code block below is the original `syncOnce` sketch, superseded by the `Run`-based test actually committed.

```go
func TestEndToEndAssignmentSync(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	caPEM, caKeyPEM, _ := ca.GenerateCA()
	srvPEM, srvKeyPEM, _ := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"}})
	s := New(q, Config{ListenAddr: "127.0.0.1:0", Timers: wireTimers(), TLS: TLSMaterial{CAPEM: caPEM, CertPEM: srvPEM, KeyPEM: srvKeyPEM}})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(runCtx)

	id := uuid.New()
	cliPEM, cliKeyPEM, _ := ca.IssueClientCert(caPEM, caKeyPEM, id, "donor")
	tlsCfg, _ := transport.ClientTLSConfig(caPEM, cliPEM, cliKeyPEM)
	tlsCfg.ServerName = "localhost"
	client := agent.NewHTTPClient("https://"+s.Addr(), tlsCfg)

	dir := t.TempDir()
	ag := agent.New(&config.Config{}, state.NewFileRegistrationStore(dir), state.NewFileStore(dir), state.NewFileAssignmentStore(dir), client, time.Hour, time.Hour)
	// register once
	regCtx, regCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	_ = ag.Run(regCtx)
	regCancel()

	// coordinator assigns two CIDs
	for _, c := range []string{"bafa", "bafb"} {
		seedBlob(t, ctx, pool, c, 7)
		assignViaSeam(t, ctx, pool, c, id)
	}

	// donor syncs and converges
	asg := state.NewFileAssignmentStore(dir)
	cur := state.NewFileStore(dir)
	c0, _ := cur.Cursor()
	c1 := ag.syncOnce(ctx, c0)
	if c1 <= c0 {
		t.Fatalf("cursor did not advance: %d→%d", c0, c1)
	}
	got, _ := asg.List()
	if len(got) != 2 {
		t.Fatalf("desired set = %d, want 2", len(got))
	}
	// NO acked rows exist — donor never acks in M3.
	var acked int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_assignments WHERE state='acked'`).Scan(&acked)
	if acked != 0 {
		t.Fatalf("acked rows = %d, want 0 (no auto-ack in M3)", acked)
	}

	// unpin one ⇒ donor removes it
	txu, _ := pool.Begin(ctx)
	UnpinPin(ctx, txu, "bafa", id)
	txu.Commit(ctx)
	ag.syncOnce(ctx, c1)
	if got, _ := asg.List(); len(got) != 1 || got[0].CID != "bafb" {
		t.Fatalf("after unpin: %+v", got)
	}

	// long-offline: prune past the cursor ⇒ next sync recovers via snapshot
	pool.Exec(ctx, `UPDATE pin_changes SET created_at = now() - interval '60 days'`)
	s.pruneOnce(ctx, time.Hour)
	ag.syncOnce(ctx, 0) // simulate a stale cursor
	if got, _ := asg.List(); len(got) != 1 || got[0].CID != "bafb" {
		t.Fatalf("after snapshot recovery: %+v", got)
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./internal/federation/coordinator/ -run EndToEndAssignmentSync -v` (with a test Postgres)
Expected: PASS — donor converges via changes and via snapshot recovery; **no acked rows**.

- [ ] **Step 3: Full suite + gates**

```bash
go build ./... && go vet ./...
go test ./...
./scripts/check_node_deps.sh
make migrations-frozen
```
Expected: all green.

- [ ] **Step 4: Commit**

```bash
git add internal/federation/coordinator/integration_test.go
git commit -m "test(p2-m3): e2e assignment sync + snapshot recovery, no auto-ack (P2-M3)"
```

---

## Task 15: Failure-mode hardening tests

**Files:**
- Modify: the relevant `*_test.go` (coordinator + agent + state)

Add the failure-mode cases the design calls out as first-class M3 deliverables (each is small; group by package):

- [ ] **Crash-after-changes-before-cursor** (agent/state): apply a batch, simulate a crash by reloading `FileAssignmentStore` + `FileStore` from disk before `SetCursor`, re-`syncOnce`, assert no double effect (idempotent) and the set is correct.
- [ ] **Assign → unpin → assign same (cid,node)** (coordinator): assert a fresh `assignment_id` + `generation=1` after the row was deleted by unpin; donor converges correctly across the three changes.
- [ ] **Seam rollback** (coordinator): `AssignPin` for a missing blob returns an error and leaves **no** `pin_assignments`/`pin_changes` row after `tx.Rollback`.
- [ ] **Snapshot 409 restart** (agent): a fake `GetSnapshot` that returns `ErrSnapshotEpochChanged` once then succeeds; assert `recoverSnapshot` restarts and converges.
- [ ] **Revoked node** (coordinator): already covered for changes (Task 4) — add snapshot/ack/fail 403 cases.
- [ ] **Fail then idempotent re-fail** (coordinator): `FailPinAssignment` 204, re-send same generation 204, older generation 409.
- [ ] **Malformed/negative query params** (coordinator): `since_seq=lol`, negative `since_seq`/`snapshot_epoch`/`limit` ⇒ 400 `bad_request` on changes/snapshot.
- [ ] **Fail-reason validation** (coordinator): empty reason normalizes to `other` (204); an unrecognized reason ⇒ 400 `bad_reason`.
- [ ] **Body/path CID mismatch** (coordinator): ack/fail with `body.cid != path cid` ⇒ 400 `cid_mismatch`.
- [ ] **Immediate first sync** (agent): startup runs one `syncOnce` before the poll ticker; assert assignments converge without waiting a full interval, and that a stale startup cursor triggers snapshot recovery immediately.

- [ ] **Run + commit**

```bash
go test ./internal/federation/coordinator/ ./internal/node/... -v
git add -A
git commit -m "test(p2-m3): change-log failure-mode hardening (crash/rollback/409/revoke) (P2-M3)"
```

---

## Task 16: Status docs

**Files:**
- Modify: `ROADMAP.md`
- Modify: `docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md` (milestone table)

- [ ] **Step 1:** Flip the P2-M3 status (and the master-design milestone-table row) to reflect completion, mirroring how M2 was recorded. Keep `ROADMAP.md` authoritative.
- [ ] **Step 2:** Final doc-link check: `python3 scripts/check_doc_links.py` (if present).
- [ ] **Step 3: Commit**

```bash
git add ROADMAP.md docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md
git commit -m "docs(p2-m3): mark assignment synchronization complete (P2-M3)"
```

---

## Self-review checklist (completed during authoring)

- **Spec coverage:** every design section maps to a task — wire reconciliation (T1), migration/queries (T2), assignment seam + advisory-lock ordering (T3), changes + `snapshot_required` (T4), snapshot + epoch 409 (T5), ack/fail generation state machine (T6), retention/prune + real `current_epoch` (T7), donor durable cursor (T8), donor desired set + idempotent apply (T9), donor client + sentinels (T10), donor pins-poll loop + fail-closed + **no ack** (T11), `cmd/node` wiring (T12), `novactl pin` (T13), e2e + snapshot recovery + no-auto-ack assertion (T14), failure-mode hardening (T15), status (T16).
- **No-fake-ack invariant:** the donor `Client` interface has **no** `Ack`/`Fail` methods; T11/T14 assert acked rows stay 0; ack/fail are coordinator-side, tested via the test client only.
- **State-name discipline:** `DesiredAssignment.State` stays `"pending"`; `novactl pin list` prints "desired assignments" vs "verified holders" separately; verified holders are empty in M3.
- **Epoch/cursor model:** global `bigserial`; advisory lock ⇒ commit-order-safe sequences, not gap-freedom (T3 concurrency test); `current_epoch` = global head; `snapshot_required` iff `since_seq < pruned_through_seq`; snapshot 409 only on this-node changes past the captured epoch; cursor adopts `snapshot_epoch` after recovery (no empty-node loop).
- **Type/identifier consistency:** `wire.PinChange/ChangesResponse/SnapshotItem/SnapshotResponse/Ack/Fail`, `wire.CodeStaleAssignment`/`ChangeKind*`/`FailReason*`, `coordinator.AssignPin/UnpinPin/Assignment`, `coordinator.Config.ChangeLogRetention/PrunePollInterval`, `state.FileStore/FileAssignmentStore/DesiredAssignment/ChangeInput/atomicWrite`, `agent.New(...)`/`Client`/`GetChanges`/`GetSnapshot`/`ErrSnapshotRequired`/`ErrSnapshotEpochChanged` used consistently across tasks.
- **sqlc identifiers:** generated `gen.GetPinChangesSinceParams/Row`, `gen.GetPinSnapshotPageParams/Row`, `gen.UpsertPinAssignmentAssignParams/Row`, `gen.AckPinAssignmentParams`, `gen.FailPinAssignmentParams`, `gen.InsertPinChangeParams`, `gen.GetPinAssignmentParams/Row`, `gen.NodeHasChangesAfterParams`, `gen.PruneChangeLog` produced in T2; T3–T6/T13 must match the actual generated names (flagged inline).
- **Robustness (review preflight):** strict query parsing (400 on malformed/negative `since_seq`/`snapshot_epoch`/`limit`); ack/fail method guards + body/path `cid_mismatch` (400); `wire.NormalizeFailReason` (empty→`other`, unknown→400); heartbeat surfaces `GetChangeLogHead` errors; `recoverSnapshot(ctx, oldCursor)` returns the old cursor on any snapshot/store error (never advances past unpersisted state); the agent runs one immediate `syncOnce` at startup; the advisory-lock guarantee is **commit-order-safe**, not gap-free.
- **Boundary:** donor-graph additions are donor-safe (`state`, `wire`, stdlib); `coordinator`/`pgx`/`uuid` stay operator-side; `cmd/novactl` importing `coordinator` is fine (operator tooling). `./scripts/check_node_deps.sh` stays green (T12).
- **Migrations:** only `0012` added; `MANIFEST.sha256` appended; `make migrations-frozen` green (T2).

## Execution

Per the milestone workflow ([[feedback-milestone-workflow]]): subagent-driven, feature branch `p2-m3-assignment-sync` (already created; design+plan committed). Execute task-by-task with a fresh subagent per task and review between tasks (superpowers:subagent-driven-development). Tasks 2/4/5/6/7/14/15 (and the `cmd/novactl` DB test) require a test Postgres (`dbtest`/testcontainers); run them where one is available. On completion: local fast-forward merge to `main` + annotated tag `p2-m3-assignment-sync`, no remote push.
