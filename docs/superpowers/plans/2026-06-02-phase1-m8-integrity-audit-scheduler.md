# M8 Integrity-Audit Scheduler Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the coordinator-internal integrity-audit subsystem (`internal/audit/integrity`): an in-process scheduler running the seven `audit_kind` checks on per-kind cadences, recording pass/fail/skip rows into the partitioned `integrity_audits` table, surfacing failures via structured logs + a `FailureSink` seam + the paginated `GET /api/v1/admin/audits/integrity` admin endpoint, plus a partition create-ahead + retention `Maintainer`.

**Architecture:** A single-goroutine `Scheduler` ticks ~10 s, fires any kind whose interval has elapsed (`lastRun` seeded at boot from `MAX(audited_at)` per kind so restarts resume mid-cadence), and runs each due kind in a per-kind no-overlap goroutine under a `context` timeout. Each `Check` samples up to `SampleSize` rows (`ORDER BY random()`), verifies them against the existing primitives (`envelope.Decode`, `keystore.Unwrap`, the decrypt pipeline, `ipfs.Backend.{Has,BlockGet}`), and returns findings; a `Recorder` batch-inserts them and, per failure, `slog.Warn`s + calls a log-only `FailureSink`. The audit deliberately bypasses `storage.Resolve`'s authz/visibility gate (it reads ciphertext+keys it owns, serves nothing). A `Maintainer` creates monthly partitions ahead of time and prunes (passes 30 d, failures ≥ 1 y). The listing endpoint introduces the repo's first pagination helper. Everything is built in `coordinator.New` (gated on pool+backend+ks) and started in `Run`, beside the M7 revocations/gcLoop/workers loops. **No new migration** (table + enums + base partitions already ship).

**Tech Stack:** Go 1.26 (per `go.mod`), stdlib `crypto/sha256` (via go-cid `Prefix().Sum`), `context`, `log/slog`, pgx/v5 (`Batch`), sqlc (`:batchexec`, `sqlc.narg`), chi, testcontainers-go (Postgres + nginx). No new third-party dependencies.

**Authoritative spec:** `docs/superpowers/specs/2026-06-02-phase1-m8-integrity-audit-scheduler-design.md` (and the normative `docs/specs/INTEGRITY_AUDIT.md`).

---

## File Structure

**Created:**
```
internal/audit/integrity/config.go             Kind enum, Cadence, DefaultCadences, EnforceAuditPolicy
internal/audit/integrity/config_test.go
internal/audit/integrity/sink.go               FailureSink interface + log-only default
internal/audit/integrity/recorder.go           Recorder: batch insert + warn log + sink dispatch
internal/audit/integrity/recorder_test.go
internal/audit/integrity/checks.go             the seven Check implementations + Finding/Check contract
internal/audit/integrity/checks_test.go
internal/audit/integrity/scheduler.go          Scheduler: tick, due-ness seed, bounded runs, RunOnce/RunKind
internal/audit/integrity/scheduler_test.go
internal/audit/integrity/retention.go          Maintainer: partition create-ahead + prune/drop
internal/audit/integrity/retention_test.go
internal/db/queries/audit.sql                  sampling + batch insert + list + count + schedule seed
internal/api/httputil/pagination.go            page/per_page parse + Pagination response
internal/api/httputil/pagination_test.go
internal/api/handlers/audits_admin.go          listIntegrityAudits handler
internal/api/handlers/audits_admin_test.go
internal/integration/m8_integrity_audit_test.go   end-to-end through nginx
```

**Modified:**
```
internal/db/gen/*                              regenerated from audit.sql
internal/api/server.go                         mount GET /admin/audits/integrity; ServerConfig.AuditAdmin
pkg/coordinator/coordinator.go                 Config.IntegrityAudit; build Scheduler+Maintainer+AuditAdmin; start in Run
cmd/coordinator/main.go                        populate IntegrityAudit defaults; NOVA_INTEGRITY_AUDIT_ENABLED
internal/config/types.go                       comment only (structs already match)
docs/specs/INTEGRITY_AUDIT.md                  reconciliations (a)–(e)
docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md  in-process model reconciliation
docs/specs/openapi.yaml                        confirm/complete listIntegrityAudits + page params
docs/ROADMAP.md                                M8 status + links + tag
```

**Build/test commands** (repo conventions): `go build ./...`, `go test ./internal/... ./pkg/... ./nova-image/... -short`, `go test ./internal/db/gen/...` after regen (codegen-check), integration via `go test ./internal/integration/ -run M8 -v` (Docker required; `-short` skips). Per the gofmt-skew note, run `gofmt -w` only on files you create/modify; do not reformat pre-existing files.

**Branch:** `m8-integrity-audit` (one branch for the milestone; finish with a local fast-forward merge + annotated tag `m8-integrity-audit`, no remote push).

---

## Task 0: sqlc queries + regenerate

**Files:**
- Create: `internal/db/queries/audit.sql`
- Modify: `internal/db/gen/*` (regen)

- [ ] **Step 1: Write `audit.sql`.** No schema change — `integrity_audits` + the `audit_kind`/`audit_result` enums already exist.

```sql
-- name: SampleEncryptedBlobs :many
SELECT b.cid, b.byte_size
FROM blobs b
WHERE b.state = 'active' AND b.encryption_key_id IS NOT NULL
ORDER BY random()
LIMIT $1;

-- name: SampleActiveBlobs :many
SELECT cid FROM blobs
WHERE state = 'active'
ORDER BY random()
LIMIT $1;

-- name: SampleDerivatives :many
SELECT d.cid, d.state::text AS state, p.state::text AS parent_state
FROM blobs d
JOIN blobs p ON p.cid = d.parent_cid
WHERE d.parent_cid IS NOT NULL
ORDER BY random()
LIMIT $1;

-- name: SampleMultiBlockBlocks :many
SELECT bb.block_cid, bb.blob_cid
FROM blob_blocks bb
JOIN blob_manifests m ON m.cid = bb.blob_cid
WHERE m.block_count > 1
ORDER BY random()
LIMIT $1;

-- name: SampleManifestConsistency :many
SELECT m.cid,
       m.block_count,
       m.envelope_size,
       count(bb.block_index)            AS actual_count,
       COALESCE(sum(bb.block_size), 0)::bigint AS actual_size
FROM blob_manifests m
LEFT JOIN blob_blocks bb ON bb.blob_cid = m.cid
WHERE m.cid IN (
    SELECT cid FROM blobs WHERE state = 'active' ORDER BY random() LIMIT $1
)
GROUP BY m.cid, m.block_count, m.envelope_size;

-- name: InsertIntegrityAudit :batchexec
INSERT INTO integrity_audits (cid, audit_kind, result, error)
VALUES ($1, $2, $3, $4);

-- name: ListIntegrityAudits :many
SELECT id, cid, audit_kind::text AS audit_kind, result::text AS result, error, audited_at
FROM integrity_audits
WHERE (sqlc.narg('result')::audit_result IS NULL OR result = sqlc.narg('result')::audit_result)
  AND (sqlc.narg('audit_kind')::audit_kind IS NULL OR audit_kind = sqlc.narg('audit_kind')::audit_kind)
ORDER BY audited_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountIntegrityAudits :one
SELECT count(*)
FROM integrity_audits
WHERE (sqlc.narg('result')::audit_result IS NULL OR result = sqlc.narg('result')::audit_result)
  AND (sqlc.narg('audit_kind')::audit_kind IS NULL OR audit_kind = sqlc.narg('audit_kind')::audit_kind);

-- name: SeedAuditSchedule :many
SELECT audit_kind::text AS audit_kind, max(audited_at)::timestamptz AS last_run
FROM integrity_audits
GROUP BY audit_kind;
```

- [ ] **Step 2: Regenerate + codegen-check.**

Run: `(cd internal/db && sqlc generate) && go build ./...`
Expected: new methods on `gen.Queries` (`InsertIntegrityAudit` returns a `*InsertIntegrityAuditBatchResults`; `audit_kind`/`result` insert params are the generated enum types). Read `internal/db/gen/models.go` + the new `audit.sql.go` to confirm the emitted enum types (likely `AuditKind` / `AuditResult` Go types with string consts) and the `pgtype` columns (`error` is `pgtype.Text`, `audited_at` is `pgtype.Timestamptz`); adapt later tasks to those exact names.

- [ ] **Step 3: Commit.**

```bash
gofmt -w internal/db/gen/   # only the regenerated files
git add internal/db/queries/audit.sql internal/db/gen
git commit -m "feat(db): integrity-audit sampling, batch insert, listing queries (sqlc)"
```

---

## Task 1: config — Kind, Cadence, DefaultCadences, EnforceAuditPolicy

**Files:**
- Create: `internal/audit/integrity/config.go`, `internal/audit/integrity/config_test.go`

- [ ] **Step 1: Write failing tests.**

```go
func TestDefaultCadencesCoverAllKinds(t *testing.T) {
    d := integrity.DefaultCadences()
    for _, k := range integrity.AllKinds {
        c, ok := d[k]
        require.True(t, ok, "missing cadence for %s", k)
        require.Positive(t, c.Interval)
        require.Positive(t, c.SampleSize)
    }
    require.Equal(t, 15*time.Minute, d[integrity.KindKuboPinPresent].Interval)
    require.Equal(t, 200, d[integrity.KindKuboPinPresent].SampleSize)
}

func TestEnforceAuditPolicyRejectsZeroInterval(t *testing.T) {
    c := map[integrity.Kind]integrity.Cadence{integrity.KindEnvelopeDecode: {Interval: 0, SampleSize: 100}}
    require.Error(t, integrity.EnforceAuditPolicy(c)) // production build refuses interval=0
}
```

- [ ] **Step 2: Run — expect FAIL** (`undefined: integrity`). `go test ./internal/audit/integrity/`.

- [ ] **Step 3: Implement `config.go`.**

```go
// Package integrity implements Nova's coordinator-internal integrity audits
// (docs/specs/INTEGRITY_AUDIT.md): local-fixity checks over the DB, the local
// Kubo blockstore, and the keystore. It is not donor-facing.
package integrity

import (
    "fmt"
    "time"
)

// Kind is one audit_kind enum value.
type Kind string

const (
    KindEnvelopeDecode            Kind = "envelope_decode"
    KindKeyUnwrap                 Kind = "key_unwrap"
    KindSampleDecrypt             Kind = "sample_decrypt"
    KindKuboPinPresent            Kind = "kubo_pin_present"
    KindDerivativeStateConsistent Kind = "derivative_state_consistent"
    KindBlockHashValid            Kind = "block_hash_valid"
    KindManifestConsistent        Kind = "manifest_consistent"
)

// AllKinds is the canonical order used for iteration and defaults.
var AllKinds = []Kind{
    KindEnvelopeDecode, KindKeyUnwrap, KindSampleDecrypt, KindKuboPinPresent,
    KindDerivativeStateConsistent, KindBlockHashValid, KindManifestConsistent,
}

// Cadence is a kind's schedule: how often it runs and how many rows it samples.
type Cadence struct {
    Interval   time.Duration
    SampleSize int
}

// DefaultCadences returns the INTEGRITY_AUDIT.md § "Schedule" defaults.
func DefaultCadences() map[Kind]Cadence {
    return map[Kind]Cadence{
        KindEnvelopeDecode:            {Interval: time.Hour, SampleSize: 100},
        KindKeyUnwrap:                 {Interval: time.Hour, SampleSize: 100},
        KindSampleDecrypt:             {Interval: time.Hour, SampleSize: 50},
        KindKuboPinPresent:            {Interval: 15 * time.Minute, SampleSize: 200},
        KindDerivativeStateConsistent: {Interval: time.Hour, SampleSize: 200},
        KindBlockHashValid:            {Interval: 24 * time.Hour, SampleSize: 100},
        KindManifestConsistent:        {Interval: 24 * time.Hour, SampleSize: 100},
    }
}

// EnforceAuditPolicy refuses dev-only configurations in production builds. A
// zero interval disables a kind, which INTEGRITY_AUDIT.md permits only under
// the nova_dev build tag. The production implementation (config.go) returns an
// error; config_dev.go (//go:build nova_dev) returns nil.
func enforceAuditPolicyProd(c map[Kind]Cadence) error {
    for k, cad := range c {
        if cad.Interval <= 0 {
            return fmt.Errorf("integrity: kind %q has interval<=0 (disabled); refused in production builds", k)
        }
        if cad.SampleSize < 1 || cad.SampleSize > 10000 {
            return fmt.Errorf("integrity: kind %q sample size %d out of bounds [1,10000]", k, cad.SampleSize)
        }
    }
    return nil
}
```

Add `config_prod.go` (`//go:build !nova_dev`) with `func EnforceAuditPolicy(c map[Kind]Cadence) error { return enforceAuditPolicyProd(c) }` and `config_dev.go` (`//go:build nova_dev`) with a permissive `EnforceAuditPolicy` that only range-checks non-zero sample sizes. (Mirrors `auth.EnforceAnonymousPolicy`.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit.**

```bash
go test ./internal/audit/integrity/ -run 'Cadence|Policy'
gofmt -w internal/audit/integrity/
git add internal/audit/integrity/config.go internal/audit/integrity/config_prod.go internal/audit/integrity/config_dev.go internal/audit/integrity/config_test.go
git commit -m "feat(audit): integrity audit kinds, cadence defaults, policy floor"
```

---

## Task 2: FailureSink + Recorder

**Files:**
- Create: `internal/audit/integrity/sink.go`, `internal/audit/integrity/recorder.go`, `internal/audit/integrity/recorder_test.go`

- [ ] **Step 1: Write failing test** (Postgres testcontainer via `dbtest`): record a mixed batch; assert one `integrity_audits` row per finding with correct `result`; assert the `FailureSink` saw exactly the failures.

```go
func TestRecorderInsertsRowsAndDispatchesFailures(t *testing.T) {
    if testing.Short() { t.Skip("integration") }
    ctx := context.Background()
    pool := dbtest.New(t, ctx)
    q := gen.New(pool)
    spy := &spySink{}
    rec := integrity.NewRecorder(q, spy, slog.Default())

    err := rec.Record(ctx, integrity.KindKuboPinPresent, []integrity.Finding{
        {CID: "bafyA", Result: integrity.ResultPass},
        {CID: "bafyB", Result: integrity.ResultFail, Detail: "not pinned"},
    })
    require.NoError(t, err)

    n, err := q.CountIntegrityAudits(ctx, gen.CountIntegrityAuditsParams{})
    require.NoError(t, err)
    require.EqualValues(t, 2, n)
    require.Equal(t, []string{"bafyB"}, spy.cids)
}
```

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `sink.go` + `recorder.go`.**

```go
// sink.go
type AuditFailure struct{ CID string; Kind Kind; Detail string }

// FailureSink is notified of each audit failure. M8 ships a log-only default;
// the integrity.audit_failed webhook (deferred) implements this later.
type FailureSink interface {
    AuditFailed(ctx context.Context, f AuditFailure)
}

type logSink struct{ log *slog.Logger }
func NewLogSink(log *slog.Logger) FailureSink { return logSink{log} }
func (s logSink) AuditFailed(_ context.Context, _ AuditFailure) {} // warn is emitted by the Recorder
```

```go
// recorder.go
const ( ResultPass = "pass"; ResultFail = "fail"; ResultSkip = "skip" )

type Finding struct{ CID, Result, Detail string }

type Recorder struct {
    q    *gen.Queries
    sink FailureSink
    log  *slog.Logger
}

func NewRecorder(q *gen.Queries, sink FailureSink, log *slog.Logger) *Recorder {
    return &Recorder{q: q, sink: sink, log: log}
}

// Record batch-inserts one integrity_audits row per finding and surfaces failures.
func (r *Recorder) Record(ctx context.Context, kind Kind, fs []Finding) error {
    if len(fs) == 0 { return nil }
    params := make([]gen.InsertIntegrityAuditParams, 0, len(fs))
    for _, f := range fs {
        params = append(params, gen.InsertIntegrityAuditParams{
            Cid:       f.CID,
            AuditKind: gen.AuditKind(kind),
            Result:    gen.AuditResult(f.Result),
            Error:     pgtype.Text{String: f.Detail, Valid: f.Detail != ""},
        })
    }
    br := r.q.InsertIntegrityAudit(ctx, params)
    defer br.Close()
    var insErr error
    br.Exec(func(_ int, err error) { if err != nil && insErr == nil { insErr = err } })
    if insErr != nil { return fmt.Errorf("integrity: record %s: %w", kind, insErr) }

    for _, f := range fs {
        if f.Result == ResultFail {
            r.log.Warn("integrity audit failed", "audit_kind", string(kind), "cid", f.CID, "error", f.Detail)
            r.sink.AuditFailed(ctx, AuditFailure{CID: f.CID, Kind: kind, Detail: f.Detail})
        }
    }
    return nil
}
```

(Adapt `gen.AuditKind`/`gen.AuditResult`/`InsertIntegrityAuditParams` field names to the actual Task-0 codegen.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(audit): recorder batch-inserts audit rows + failure sink seam`).

---

## Task 3: the seven checks

**Files:**
- Create: `internal/audit/integrity/checks.go`, `internal/audit/integrity/checks_test.go`

- [ ] **Step 1: Write failing tests** (`dbtest` + `blobfixture` + an offline embedded backend, or a small fake `Backend`): a clean encrypted blob passes `envelope_decode`/`key_unwrap`/`sample_decrypt`; an unpinned CID fails `kubo_pin_present`; a single-block blob is `skip` for `block_hash_valid` and a real multi-block block passes; a manifest with a wrong `block_count` fails `manifest_consistent`; a derivative whose parent is `quarantined` while it is `active` fails `derivative_state_consistent`; a `public_archival` (unencrypted) blob is `skip` for the crypto checks.

```go
func TestKuboPinPresentFailsWhenUnpinned(t *testing.T) {
    if testing.Short() { t.Skip("integration") }
    // seed an active blob whose CID the fake backend reports Has=false
    findings, err := chk.Run(ctx, 10)
    require.NoError(t, err)
    require.Contains(t, results(findings), integrity.ResultFail)
}
```

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `checks.go`.** The contract and the seven bodies:

```go
// Backend is the subset of ipfs.Backend the checks need (kept narrow for fakes).
type Backend interface {
    Get(ctx context.Context, c cid.Cid) (io.ReadCloser, error)
    Has(ctx context.Context, c cid.Cid) (bool, error)
    BlockGet(ctx context.Context, c cid.Cid) ([]byte, error)
}

type Check interface {
    Kind() Kind
    Run(ctx context.Context, sampleSize int) ([]Finding, error)
}

type deps struct {
    q       *gen.Queries
    backend Backend
    ks      *envelope.Keystore
    maxDecryptBytes int64 // sample_decrypt size cap
}
```

`envelope_decode` — header only:
```go
rc, err := d.backend.Get(ctx, c)
if err != nil { return fail("get: " + err.Error()) }
hdr := make([]byte, envelope.HeaderSize)
_, rerr := io.ReadFull(rc, hdr)
rc.Close()
if rerr != nil { return fail("read header: " + rerr.Error()) }
if _, _, derr := envelope.Decode(hdr); derr != nil { return fail(derr.Error()) }
return pass()
```

`key_unwrap`:
```go
dek, err := d.q.GetDEKByBlob(ctx, cidStr)
if err != nil { return fail("get dek: " + err.Error()) }
if dek.State == "shredded" { return skip() }
mkv, err := uuid.Parse(dek.MasterKeyVersionID)
if err != nil { return fail("mkv parse: " + err.Error()) }
if _, err := d.ks.Unwrap(ctx, dek.WrappedKey, mkv); err != nil { return fail(err.Error()) }
return pass()
```

`sample_decrypt` (whole-envelope; size-capped):
```go
if byteSize > d.maxDecryptBytes { return skip() }
rc, err := d.backend.Get(ctx, c); ... ; env, _ := io.ReadAll(rc); rc.Close()
dek, _ := d.q.GetDEKByBlob(ctx, cidStr)
key, err := d.ks.Unwrap(ctx, dek.WrappedKey, mkv); if err != nil { return fail(err.Error()) }
_, codec, err := envelope.Decode(env); if err != nil { return fail(err.Error()) }
if _, err := codec.Decrypt(env, key); err != nil { return fail(err.Error()) } // ErrEnvelopeAuthFailed
return pass()
```

`kubo_pin_present`:
```go
ok, err := d.backend.Has(ctx, c)
if err != nil { return fail("has: " + err.Error()) }
if !ok { return fail("not pinned") }
return pass()
```

`block_hash_valid` — re-derive the CID from the bytes:
```go
stored, err := cid.Decode(row.BlockCid); if err != nil { return fail("decode cid: " + err.Error()) }
raw, err := d.backend.BlockGet(ctx, stored); if err != nil { return fail("block get: " + err.Error()) }
got, err := stored.Prefix().Sum(raw); if err != nil { return fail("rehash: " + err.Error()) }
if !got.Equals(stored) { return fail("hash mismatch") }
return pass()
```

`manifest_consistent`:
```go
if row.BlockCount != int32(row.ActualCount) || row.EnvelopeSize != row.ActualSize {
    return fail(fmt.Sprintf("manifest block_count=%d/blocks=%d envelope_size=%d/sum=%d",
        row.BlockCount, row.ActualCount, row.EnvelopeSize, row.ActualSize))
}
return pass()
```

`derivative_state_consistent`:
```go
// A derivative must not be more available than its parent. Phase 1: fail when the
// parent is not active but the derivative still is.
if row.ParentState != "active" && row.State == "active" {
    return fail("parent state " + row.ParentState + " but derivative active")
}
return pass()
```

(Each check samples via its Task-0 query, loops, and collects `Finding`s. `public_archival`/unencrypted blobs never appear in `SampleEncryptedBlobs`, so the crypto checks naturally only see encrypted blobs; record `skip` only where a sampled row turns out inapplicable.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(audit): seven integrity check implementations`).

---

## Task 4: Scheduler

**Files:**
- Create: `internal/audit/integrity/scheduler.go`, `internal/audit/integrity/scheduler_test.go`

- [ ] **Step 1: Write failing tests** (fake clock + fake checks): a kind runs when due and not before; `lastRun` seeded from `SeedAuditSchedule` defers a recently-run kind; a disabled kind (interval 0, dev build) never runs; `runKind` does not overlap itself; a check that blocks past its budget is cancelled; `RunOnce` runs every enabled kind exactly once.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `scheduler.go`.**

```go
type Scheduler struct {
    checks   map[Kind]Check
    cadences map[Kind]Cadence
    rec      *Recorder
    q        *gen.Queries
    log      *slog.Logger
    now      func() time.Time
    tick     time.Duration            // default 10s
    budget   func(Kind) time.Duration // per-run timeout

    mu      sync.Mutex
    lastRun map[Kind]time.Time
    running map[Kind]bool
}

func NewScheduler(checks map[Kind]Check, cadences map[Kind]Cadence, rec *Recorder, q *gen.Queries, log *slog.Logger) *Scheduler { /* defaults: now=time.Now, tick=10s, budget=interval or 5m cap */ }

func (s *Scheduler) Run(ctx context.Context) {
    s.seed(ctx)
    t := time.NewTicker(s.tick); defer t.Stop()
    for {
        select {
        case <-ctx.Done(): return
        case <-t.C:
            for _, k := range AllKinds {
                if s.due(k) { go s.runKind(ctx, k) }
            }
        }
    }
}

func (s *Scheduler) seed(ctx context.Context) {
    rows, err := s.q.SeedAuditSchedule(ctx)
    if err != nil { s.log.Warn("integrity: seed schedule", "err", err); return }
    s.mu.Lock(); defer s.mu.Unlock()
    for _, r := range rows { s.lastRun[Kind(r.AuditKind)] = r.LastRun.Time }
}

func (s *Scheduler) due(k Kind) bool {
    c, ok := s.cadences[k]; if !ok || c.Interval <= 0 { return false }
    s.mu.Lock(); defer s.mu.Unlock()
    if s.running[k] { return false }
    return s.now().Sub(s.lastRun[k]) >= c.Interval
}

func (s *Scheduler) runKind(ctx context.Context, k Kind) {
    s.mu.Lock(); if s.running[k] { s.mu.Unlock(); return }; s.running[k] = true; s.mu.Unlock()
    defer func() { s.mu.Lock(); s.running[k] = false; s.lastRun[k] = s.now(); s.mu.Unlock() }()

    cctx, cancel := context.WithTimeout(ctx, s.budget(k)); defer cancel()
    findings, err := s.checks[k].Run(cctx, s.cadences[k].SampleSize)
    if err != nil { s.log.Warn("integrity: check run", "audit_kind", string(k), "err", err); return }
    if err := s.rec.Record(cctx, k, findings); err != nil { s.log.Warn("integrity: record", "audit_kind", string(k), "err", err) }
}

// RunOnce runs every enabled kind synchronously (tests + a future run-now).
func (s *Scheduler) RunOnce(ctx context.Context) { for _, k := range AllKinds { if c, ok := s.cadences[k]; ok && c.Interval > 0 { s.runKindSync(ctx, k) } } }
func (s *Scheduler) RunKind(ctx context.Context, k Kind) { s.runKindSync(ctx, k) }
```

(`runKindSync` is `runKind` without the `go`; share the body.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(audit): in-process scheduler with per-kind cadence + bounded runs`).

---

## Task 5: retention Maintainer

**Files:**
- Create: `internal/audit/integrity/retention.go`, `internal/audit/integrity/retention_test.go`

- [ ] **Step 1: Write failing test** (`dbtest`): `maintain` creates a partition for next month (assert via `pg_catalog`/`to_regclass('integrity_audits_YYYY_MM')` non-null); inserting an `audited_at` in next month then succeeds; pass rows older than `passRetention` are deleted while failures survive; a manually-created aged partition is dropped.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `retention.go`.**

```go
type Maintainer struct {
    pool    *pgxpool.Pool
    passRet time.Duration // default 30d
    failRet time.Duration // default 365d
    now     func() time.Time
    log     *slog.Logger
}

func (m *Maintainer) Run(ctx context.Context) {
    m.maintain(ctx)
    t := time.NewTicker(24 * time.Hour); defer t.Stop()
    for { select { case <-ctx.Done(): return; case <-t.C: m.maintain(ctx) } }
}

func (m *Maintainer) maintain(ctx context.Context) {
    if err := m.ensurePartitions(ctx); err != nil { m.log.Warn("integrity: ensure partitions", "err", err) }
    if err := m.prunePasses(ctx); err != nil { m.log.Warn("integrity: prune passes", "err", err) }
    if err := m.dropAged(ctx); err != nil { m.log.Warn("integrity: drop aged partitions", "err", err) }
}

// ensurePartitions creates this month + the next two (idempotent). Names are
// month-derived (no user input), so fmt.Sprintf DDL is safe.
func (m *Maintainer) ensurePartitions(ctx context.Context) error {
    base := time.Date(m.now().Year(), m.now().Month(), 1, 0, 0, 0, 0, time.UTC)
    for i := 0; i < 3; i++ {
        start := base.AddDate(0, i, 0)
        end := start.AddDate(0, 1, 0)
        name := fmt.Sprintf("integrity_audits_%04d_%02d", start.Year(), int(start.Month()))
        ddl := fmt.Sprintf(
            "CREATE TABLE IF NOT EXISTS %s PARTITION OF integrity_audits FOR VALUES FROM ('%s') TO ('%s')",
            name, start.Format("2006-01-02"), end.Format("2006-01-02"))
        if _, err := m.pool.Exec(ctx, ddl); err != nil { return fmt.Errorf("%s: %w", name, err) }
    }
    return nil
}

func (m *Maintainer) prunePasses(ctx context.Context) error {
    _, err := m.pool.Exec(ctx,
        `DELETE FROM integrity_audits WHERE result = 'pass' AND audited_at < $1`,
        m.now().Add(-m.passRet))
    return err
}
```

`dropAged` selects child partitions from `pg_inherits`/`pg_class` whose upper bound is older than `now-failRet` and `DROP TABLE`s them. (Parse the partition bound, or derive month bounds from the `integrity_audits_YYYY_MM` name; either is fine — derive from the name to avoid bound parsing.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(audit): partition create-ahead + retention pruning maintainer`).

---

## Task 6: pagination helper + admin listing handler

**Files:**
- Create: `internal/api/httputil/pagination.go`(+`_test.go`), `internal/api/handlers/audits_admin.go`(+`_test.go`)

- [ ] **Step 1: Write failing tests.** `ParsePage` defaults (1/50), caps `per_page` at 100, rejects `page=0`/negatives → error; the handler returns `{data, pagination{page,per_page,total}}`, honours `result`/`audit_kind` filters, and 400s an invalid filter.

```go
func TestParsePageDefaultsAndCaps(t *testing.T) {
    p, err := httputil.ParsePage(httptest.NewRequest("GET", "/x?per_page=500", nil))
    require.NoError(t, err)
    require.Equal(t, 1, p.Page); require.Equal(t, 100, p.PerPage)
    require.Equal(t, 100, p.Limit); require.Equal(t, 0, p.Offset)
}
```

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement.**

```go
// pagination.go
type Page struct{ Page, PerPage, Limit, Offset int }
type Pagination struct {
    Page    int `json:"page"`
    PerPage int `json:"per_page"`
    Total   int `json:"total"`
}
func ParsePage(r *http.Request) (Page, error) {
    page, perPage := 1, 50
    if v := r.URL.Query().Get("page"); v != "" {
        n, err := strconv.Atoi(v); if err != nil || n < 1 { return Page{}, errInvalidPage }
        page = n
    }
    if v := r.URL.Query().Get("per_page"); v != "" {
        n, err := strconv.Atoi(v); if err != nil || n < 1 { return Page{}, errInvalidPage }
        if n > 100 { n = 100 }
        perPage = n
    }
    return Page{Page: page, PerPage: perPage, Limit: perPage, Offset: (page - 1) * perPage}, nil
}
```

```go
// audits_admin.go
type AuditAdminHandler struct{ q *gen.Queries }
func NewAuditAdminHandler(q *gen.Queries) *AuditAdminHandler { return &AuditAdminHandler{q: q} }

func (h *AuditAdminHandler) List(w http.ResponseWriter, r *http.Request) {
    rid := middleware.RequestIDFromContext(r.Context())
    pg, err := httputil.ParsePage(r)
    if err != nil { httputil.WriteError(w, 400, "invalid_request", err.Error(), rid); return }
    result, ok := optionalEnum(r, "result", validResults)
    if !ok { httputil.WriteError(w, 400, "invalid_request", "bad result", rid); return }
    kind, ok := optionalEnum(r, "audit_kind", validKinds)
    if !ok { httputil.WriteError(w, 400, "invalid_request", "bad audit_kind", rid); return }
    // build ListIntegrityAuditsParams{Result: result(narg), AuditKind: kind(narg), Lim, Off}; CountIntegrityAuditsParams{...}
    // marshal {data: rows-mapped-to-IntegrityAudit, pagination: {pg.Page, pg.PerPage, total}}
}
```

(`validResults = {pass,fail,skip}`, `validKinds =` the seven; empty filter ⇒ NULL narg. Map rows to the openapi `IntegrityAudit` shape: `id, cid, audit_kind, result, error, audited_at`.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(api): paginated integrity-audit listing handler + pagination helper`).

---

## Task 7: mount the route

**Files:**
- Modify: `internal/api/server.go`

- [ ] **Step 1: Add `AuditAdmin *handlers.AuditAdminHandler` to `ServerConfig`** (nil ⇒ endpoint 404s via `adminNotFound`), beside `SigningAdmin`.

- [ ] **Step 2: Mount inside the `/api/v1/admin` group** (which already carries `RequireRole("operator","moderator")`), before the `adminNotFound` wildcard:

```go
if cfg.AuditAdmin != nil {
    r.Get("/audits/integrity", cfg.AuditAdmin.List)
}
```

- [ ] **Step 3: Build + unit.** `go build ./... && go test ./internal/api/... -short`. Expected PASS.

- [ ] **Step 4: gofmt; commit** (`feat(api): mount GET /api/v1/admin/audits/integrity`).

---

## Task 8: coordinator + cmd wiring

**Files:**
- Modify: `pkg/coordinator/coordinator.go`, `cmd/coordinator/main.go`, `internal/config/types.go`

- [ ] **Step 1: Config.** Add to `coordinator.Config`:

```go
// IntegrityAudit tunes the M8 audit scheduler. Zero Cadences ⇒ DefaultCadences().
type IntegrityAuditConfig struct {
    Enabled       bool
    Cadences      map[integrity.Kind]integrity.Cadence
    PassRetention time.Duration // default 30d
    FailRetention time.Duration // default 365d
}
```

- [ ] **Step 2: Build in `coordinator.New`** (when `pool != nil && backend != nil && ks != nil`): construct the seven checks over `gen.New(pool)` + `backend` + `ks`; `rec := integrity.NewRecorder(q, integrity.NewLogSink(slog.Default()), slog.Default())`; `c.auditScheduler = integrity.NewScheduler(checks, cadencesOrDefault, rec, q, slog.Default())`; `c.auditMaintainer = integrity.NewMaintainer(pool, passRet, failRet)`. Set `sc.AuditAdmin = handlers.NewAuditAdminHandler(gen.New(pool))`.

- [ ] **Step 3: Start in `Run`** beside the other goroutines:

```go
if c.auditScheduler != nil && c.cfg.IntegrityAudit.Enabled {
    go c.auditScheduler.Run(ctx)
    go c.auditMaintainer.Run(ctx)
}
```

- [ ] **Step 4: cmd defaults.** In `cmd/coordinator/main.go`, populate `IntegrityAudit: coordinator.IntegrityAuditConfig{ Enabled: os.Getenv("NOVA_INTEGRITY_AUDIT_ENABLED") != "false", Cadences: integrity.DefaultCadences(), PassRetention: 30*24*time.Hour, FailRetention: 365*24*time.Hour }` and document `NOVA_INTEGRITY_AUDIT_ENABLED` in the header comment. Add a comment to `internal/config/types.go` that M8 consumes these defaults via `coordinator.Config` (operator.yaml decode still deferred).

- [ ] **Step 5: Build + unit suite.** `go build ./... && go test ./internal/... ./pkg/... -short`. Expected PASS.

- [ ] **Step 6: gofmt; commit** (`feat(coordinator,cmd): wire integrity-audit scheduler + maintainer + admin handler`).

---

## Task 9: integration test — end-to-end through nginx

**Files:**
- Create: `internal/integration/m8_integrity_audit_test.go`

- [ ] **Step 1: Implement** the design § "Integration" scenario, reusing the M7 nginx + Postgres testcontainer harness and the operator-login helper. Build the coordinator with fast cadences (or call `scheduler.RunOnce`/`RunKind` directly via an exported test seam). Steps: upload encrypted + `public_archival` + multi-block (>1 MiB) + derivative blobs; `RunOnce`; `GET /api/v1/admin/audits/integrity` shows pass/skip rows; `backend.Unpin` a known CID → `RunKind(KindKuboPinPresent)` → `GET …?result=fail&audit_kind=kubo_pin_present` returns that CID; pagination + filters; authz (operator/moderator 200, no token 401, bad role 403); assert next-month partition exists.

- [ ] **Step 2: Run** `go test ./internal/integration/ -run M8 -v`. Expected PASS (Docker required; `-short` skips).

- [ ] **Step 3: Commit** (`test(m8): end-to-end integrity audit scheduler + listing through nginx`).

---

## Task 10: documentation reconciliations

**Files:**
- Modify: `docs/specs/INTEGRITY_AUDIT.md`, `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md`, `docs/specs/openapi.yaml`, `docs/ROADMAP.md`

- [ ] **Step 1: INTEGRITY_AUDIT.md** — reconciliations (a)–(e) from the design § "Source of truth": metric deferred; webhook deferred (FailureSink seam); `sample_decrypt` whole-envelope v1 clarification; `derivative_state_consistent` sampling-vs-"past hour" note; concrete retention mechanism (monthly create-ahead + pass DELETE@30d + partition DROP@≥1y).

- [ ] **Step 2: master MVP design** (`2026-05-25-…-design.md` lines ~511–530, ~929–934) — reconcile the "Integrity audit loop" sketch + M8 bullet from "scheduler ENQUEUEs `jobs.integrity_audit_run`; worker runs" to the in-process model.

- [ ] **Step 3: openapi.yaml** — confirm `listIntegrityAudits` matches the handler; ensure `PageParam`/`PerPageParam` components exist (add if missing); document `401 unauthenticated` / `403 forbidden` admin responses.

- [ ] **Step 4: ROADMAP** — mark M8 status; link this design + plan.

- [ ] **Step 5: Validate + commit.**

```bash
npx --yes @redocly/cli lint docs/specs/openapi.yaml 2>&1 | tail -5 || echo "(no redocly; skipping)"
git add docs/specs/INTEGRITY_AUDIT.md docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md docs/specs/openapi.yaml docs/ROADMAP.md
git commit -m "docs(m8): reconcile in-process model, deferred metric/webhook, retention, openapi"
```

---

## Task 11: full suite + finish the branch

- [ ] **Step 1: Full unit + short suite.** `go build ./... && go test ./... -short`. Expected PASS. (Per the gofmt-skew note, `gofmt -l` may flag pre-existing files; ensure only files you touched are formatted.)

- [ ] **Step 2: M8 integration.** `go test ./internal/integration/ -run M8 -v`. Expected PASS.

- [ ] **Step 3: codegen-check.** `(cd internal/db && sqlc generate) && git diff --exit-code internal/db/gen` (clean).

- [ ] **Step 4: Untagged build is the verified path.** `go build -o /tmp/nova-coordinator ./cmd/coordinator && echo OK`.

- [ ] **Step 5: Finish the branch.** Use `superpowers:finishing-a-development-branch` → fast-forward merge `m8-integrity-audit` into `main` + annotated tag `m8-integrity-audit` (local; no remote push), per the milestone workflow.

---

## Notes for the implementer

- **TDD discipline:** every package task is failing-test → run-FAIL → implement → run-PASS → commit. Do not batch.
- **Conform to `INTEGRITY_AUDIT.md` exactly** (it is normative): the seven kinds, their cadences/sample sizes, `pass|fail|skip` semantics, and the retention policy. Drift is a bug.
- **In-process, not the job queue:** the scheduler runs checks inline in bounded goroutines under `context` timeouts — it does **not** enqueue `jobs.*`. Keep `internal/audit/integrity` free of any `internal/jobs` import (the master-design enqueue sketch is reconciled, not implemented).
- **Bypass authz deliberately:** the checks read via `backend`/`keystore`/`envelope` directly, never `storage.Resolve`/`OpenBytes` (which gate on visibility and are for outbound serving). The audit must see private content and must never emit bytes.
- **Never log secrets:** failure logs carry `cid` + `audit_kind` + an error string — never key material, plaintext, or envelope bytes.
- **Generated-type drift:** after Task 0, read `internal/db/gen/audit.sql.go` + `models.go` — `audit_kind`/`result` are likely generated enum types (`AuditKind`/`AuditResult` with string consts), `error` is `pgtype.Text`, `audited_at`/`last_run` are `pgtype.Timestamptz`; `InsertIntegrityAudit` is a `:batchexec` returning `*…BatchResults`. Adapt the Recorder + handler call sites to the exact emitted names. The `narg` filter params are `pgtype`-wrapped / pointer types — pass nil/invalid for "no filter".
- **Partition cliff is real:** the committed partitions end at **2026-07-01**. The Maintainer must run at boot (it does) so July's partition exists before the scheduler inserts a `now()` row in July. Don't gate the Maintainer behind `Enabled` only — keep create-ahead running whenever the audit subsystem is constructed.
- **`block_hash_valid` re-hash:** use `storedCID.Prefix().Sum(blockBytes)` then `got.Equals(stored)`; the prefix carries codec + multihash so this re-derives spec-correct CIDs without hardcoding the algorithm. Single-block blobs (no `block_count>1` rows) never appear in the sample → nothing to skip there; record `skip` only if a sampled block's blob turns out single-block.
- **`sample_decrypt` cost:** v1 is single-shot, so the whole sampled envelope is decrypted (the spec's "byte range" is Phase-2 streaming). Cap by `byte_size` (a few MiB) and keep the sample small (50); oversized → `skip`.
- **Sampling cost:** `ORDER BY random()` is acceptable for Phase-1 corpora; do **not** prematurely switch to `TABLESAMPLE` (documented scale path only).
- **Test isolation:** seed `master_key_versions` before DEKs/blobs (FK); reuse the M2/M6 keystore + `blobfixture` helpers; the offline embedded backend (`Online:false`) is the cheap path for fixity tests, or use a narrow fake `Backend`.
- **Forward seam (not M8 code):** the `FailureSink` interface is where the deferred `integrity.audit_failed` webhook and any future `nova_integrity_audit_failures_total` metric attach — keep the interface stable and the log-only default in place.
