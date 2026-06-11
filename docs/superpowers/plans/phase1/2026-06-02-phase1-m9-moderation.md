# M9 Moderation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement Nova's moderation backbone: an `internal/moderation` core (quarantine, tombstone + crypto-shred + derivative cascade, clear-legal-hold, restore, counter-notice) driven by an in-process scheduled-tombstone sweep; a two-mode `internal/auditlog` writer + `GET /api/v1/admin/audit-log`; a public `POST /legal/dmca` intake and the `/api/v1/admin/moderation/*` admin surface; an operator-curated CID blocklist enforced at the read + import paths; and a `novactl moderation` subcommand — proving the three exit criteria end-to-end.

**Architecture:** Each moderation operation runs in one `pgx.Tx` and writes its `audit_log` row via `auditW.WriteTx` **inside** that tx (the trail commits with the action). Tombstone sets `blobs.state='tombstoned'`, cascades derivative state through the M5 `product.OnDelete` seam, set-based-shreds the parent + derivative DEKs (the `no_shred_under_legal_hold` CHECK refuses a `legal_hold=true` shred → mapped to `ErrLegalHold`, tx rolls back), inserts a `cid` signed-URL revocation, then best-effort `backend.Unpin`s after commit. A ~1-min in-process `Sweeper` tombstones overdue quarantines (sibling of `gcLoop`/the M8 `Maintainer`; counter-notice just clears `scheduled_tombstone_at`). The blocklist is a direct indexed `IsBlocklisted` PK lookup (no cache). Restore leaves revocations intact (the table has no issuer column). Everything is built in `coordinator.New` (gated on pool+backend) and started in `Run`. **One new migration** (the `blocklist` table); every other table already ships.

**Tech Stack:** Go 1.26 (per `go.mod`), pgx/v5 (`Begin`/`WithTx`, `pgconn.PgError` for the CHECK), `encoding/json` (jsonb payloads), `log/slog`, sqlc (`:one`/`:many`/`:exec`, `sqlc.narg`/`sqlc.arg`), chi, testcontainers-go (Postgres + nginx). No new third-party dependencies.

**Authoritative spec:** `docs/superpowers/specs/phase1/2026-06-02-phase1-m9-moderation-design.md` (and the normative `docs/legal/DMCA_PROCEDURE.md` + `docs/legal/SEVERE_CONTENT_PROCEDURE.md`).

---

## File Structure

**Created:**
```
internal/db/migrations/0008_moderation.sql        blocklist table
internal/db/queries/moderation.sql                decisions, state/DEK mutations, cases, blocklist, infringer, overdue-sweep
internal/auditlog/writer.go                        two-mode Writer + Entry
internal/auditlog/writer_test.go
internal/moderation/errors.go                      ErrLegalHold, ErrNotQuarantined, ErrBlobNotFound, ...
internal/moderation/service.go                     Service + Quarantine/Tombstone/ClearLegalHold/Restore/CounterNotice + CascadeHook seam
internal/moderation/service_test.go
internal/moderation/sweeper.go                     in-process ≈1m loop
internal/moderation/sweeper_test.go
internal/api/handlers/dmca_intake.go               public POST /legal/dmca
internal/api/handlers/dmca_intake_test.go
internal/api/handlers/moderation_admin.go          /api/v1/admin/moderation/* (actions, queue, cases, blocklist)
internal/api/handlers/moderation_admin_test.go
internal/api/handlers/audit_log_admin.go           GET /api/v1/admin/audit-log
internal/api/handlers/audit_log_admin_test.go
internal/integration/m9_moderation_test.go         end-to-end through nginx
```

**Modified:**
```
internal/db/queries/audit.sql                      + InsertAuditLog / ListAuditLog / CountAuditLog
internal/db/gen/*                                  regenerated
docs/specs/DATA_MODEL.sql                          + blocklist; audit_log create-ahead note
internal/audit/integrity/retention.go              extract monthly create-ahead helper; cover audit_log
internal/api/server.go                             mount /legal/dmca + /admin/moderation/* + /admin/audit-log; ServerConfig pointers
internal/api/handlers/signing_admin.go             best-effort audit Write on rotate/revoke/sign (M7 backfill)
internal/api/handlers/upload.go                    map storage.ErrBlobBlocklisted → 451
pkg/coordinator/coordinator.go                     ModerationConfig; build Service/Sweeper/handlers; productHook.OnDelete; start sweep
pkg/coordinator/producthook.go                     OnDelete cascade adapter
pkg/coordinator/storage/blob.go                    IsBlocklisted in Resolve
pkg/coordinator/storage/put.go                     IsBlocklisted before commit
pkg/coordinator/storage/errors.go                  + ErrBlobBlocklisted
cmd/coordinator/main.go                            ModerationConfig defaults; NOVA_MODERATION_SWEEP_ENABLED
cmd/novactl/main.go                                moderation subcommand group + usage
docs/specs/openapi.yaml                            reconciliations #1
docs/legal/DMCA_PROCEDURE.md                       reconciliation #2
docs/legal/SEVERE_CONTENT_PROCEDURE.md             reconciliation #3
docs/specs/INTEGRITY_AUDIT.md                      reconciliation #5
docs/ROADMAP.md                                    reconciliation #6 (status + tag + deferrals)
```

**Build/test commands** (repo conventions): `go build ./...`, `go test ./internal/... ./pkg/... ./nova-image/... -short`, `make sqlc-generate` then `git diff --exit-code internal/db/gen` (codegen-check), integration via `go test ./internal/integration/ -run M9 -v` (Docker required; `-short` skips). Per the gofmt-skew note, run `gofmt -w` only on files you create/modify.

**Branch:** `m9-moderation` (one branch for the milestone; finish with a local fast-forward merge + annotated tag `m9-moderation`, no remote push).

---

## Task 0: migration + sqlc queries + regenerate

**Files:**
- Create: `internal/db/migrations/0008_moderation.sql`, `internal/db/queries/moderation.sql`
- Modify: `docs/specs/DATA_MODEL.sql`, `internal/db/queries/audit.sql`, `internal/db/gen/*` (regen)

- [ ] **Step 1: Write the migration.** Only the `blocklist` table is new.

```sql
-- 0008_moderation.sql
-- +goose Up
CREATE TABLE blocklist (
    cid         text PRIMARY KEY,
    reason      text NOT NULL,
    rule        moderation_rule NOT NULL DEFAULT 'operator_manual',
    added_by    uuid REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX blocklist_created_at_idx ON blocklist (created_at DESC);
```

(Match the existing migrations' goose pragma style — check `0007_refresh_token_gc_index.sql` for the exact `-- +goose Up`/`Down` header used; forward-only, so a no-op or `DROP TABLE blocklist` Down is fine per repo convention.) Mirror the table into `docs/specs/DATA_MODEL.sql` after the `signed_url_revocations` section.

- [ ] **Step 2: Write `internal/db/queries/moderation.sql`.**

```sql
-- name: GetBlobForModeration :one
SELECT owner_id::text AS owner_id, state::text AS state, (encryption_key_id IS NOT NULL) AS encrypted
FROM blobs WHERE cid = $1;

-- name: SetBlobState :exec
UPDATE blobs SET state = $2 WHERE cid = $1;

-- name: ListDerivativeCIDs :many
SELECT cid FROM blobs WHERE parent_cid = $1;

-- name: ShredDEKsForBlobTree :exec
-- Parent (b.cid=$1) + derivatives (b.parent_cid=$1) in one statement; the
-- no_shred_under_legal_hold CHECK raises (23514) if any target has legal_hold=true.
UPDATE data_encryption_keys k
SET state = 'shredded', wrapped_key = sqlc.arg('zeros'), shredded_at = now()
FROM blobs b
WHERE b.encryption_key_id = k.id AND (b.cid = sqlc.arg('cid') OR b.parent_cid = sqlc.arg('cid'));

-- name: SetDEKLegalHoldForBlobTree :exec
UPDATE data_encryption_keys k
SET legal_hold = sqlc.arg('hold')
FROM blobs b
WHERE b.encryption_key_id = k.id AND (b.cid = sqlc.arg('cid') OR b.parent_cid = sqlc.arg('cid'));

-- name: InsertModerationDecision :one
INSERT INTO moderation_decisions (cid, rule, rule_ref, action, decided_by, scheduled_tombstone_at, legal_hold, notes)
VALUES (sqlc.arg('cid'), sqlc.arg('rule'), sqlc.narg('rule_ref'), sqlc.arg('action'),
        sqlc.narg('decided_by'), sqlc.narg('scheduled_tombstone_at'), sqlc.arg('legal_hold'), sqlc.narg('notes'))
RETURNING id;

-- name: ClearScheduledTombstone :exec
UPDATE moderation_decisions SET scheduled_tombstone_at = NULL
WHERE cid = $1 AND scheduled_tombstone_at IS NOT NULL;

-- name: ClearModerationLegalHold :exec
UPDATE moderation_decisions SET legal_hold = false, scheduled_tombstone_at = now()
WHERE cid = $1 AND legal_hold = true;

-- name: ListOverdueTombstones :many
SELECT md.cid, md.rule::text AS rule, md.rule_ref
FROM moderation_decisions md
JOIN blobs b ON b.cid = md.cid
LEFT JOIN data_encryption_keys k ON k.id = b.encryption_key_id
WHERE md.scheduled_tombstone_at IS NOT NULL
  AND md.scheduled_tombstone_at < now()
  AND md.action = 'quarantine'
  AND b.state = 'quarantined'
  AND (k.legal_hold IS NULL OR k.legal_hold = false);

-- name: ListModerationDecisions :many
SELECT id, cid, rule::text AS rule, rule_ref, action::text AS action, decided_by::text AS decided_by,
       decided_at, scheduled_tombstone_at, legal_hold, notes
FROM moderation_decisions
ORDER BY decided_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountModerationDecisions :one
SELECT count(*) FROM moderation_decisions;

-- name: UpsertRepeatInfringer :exec
INSERT INTO takedown_repeat_infringers (user_id, strikes, last_strike_at)
VALUES ($1, 1, now())
ON CONFLICT (user_id) DO UPDATE
SET strikes = takedown_repeat_infringers.strikes + 1, last_strike_at = now();

-- name: InsertDMCACase :one
INSERT INTO dmca_cases (claimant_name, claimant_email, sworn_statement, target_cid)
VALUES ($1, $2, $3, $4) RETURNING id;

-- name: GetDMCACase :one
SELECT id, claimant_name, claimant_email, sworn_statement, target_cid, received_at, actioned_at, status::text AS status
FROM dmca_cases WHERE id = $1;

-- name: ListDMCACases :many
SELECT id, claimant_name, claimant_email, target_cid, received_at, actioned_at, status::text AS status
FROM dmca_cases ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountDMCACases :one
SELECT count(*) FROM dmca_cases;

-- name: SetDMCACaseActioned :exec
UPDATE dmca_cases SET status = 'actioned', actioned_at = now() WHERE id = $1;

-- name: InsertBlocklist :exec
INSERT INTO blocklist (cid, reason, rule, added_by) VALUES ($1, $2, $3, $4)
ON CONFLICT (cid) DO NOTHING;

-- name: DeleteBlocklist :exec
DELETE FROM blocklist WHERE cid = $1;

-- name: ListBlocklist :many
SELECT cid, reason, rule::text AS rule, added_by::text AS added_by, created_at
FROM blocklist ORDER BY created_at DESC LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountBlocklist :one
SELECT count(*) FROM blocklist;

-- name: IsBlocklisted :one
SELECT EXISTS(SELECT 1 FROM blocklist WHERE cid = $1);
```

> **Note on `dmca_cases.notes`:** the schema has no `notes` column on `dmca_cases`, and no `dmca_status` value cleanly represents a counter-notice. Counter-notice context is therefore stored as a `moderation_decisions` row (rule `dmca`, `notes`=the counter text); the `dmca_cases` row is only advanced to `actioned` when a takedown fires (`SetDMCACaseActioned`).

- [ ] **Step 3: Append to `internal/db/queries/audit.sql`** (the M8 file):

```sql
-- name: InsertAuditLog :exec
INSERT INTO audit_log (actor_id, action, target_type, target_id, payload)
VALUES (sqlc.narg('actor_id'), $2, $3, $4, $5);

-- name: ListAuditLog :many
SELECT id, actor_id::text AS actor_id, action, target_type, target_id, payload, at
FROM audit_log
WHERE (sqlc.narg('action')::text IS NULL OR action = sqlc.narg('action')::text)
  AND (sqlc.narg('target_type')::text IS NULL OR target_type = sqlc.narg('target_type')::text)
ORDER BY at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountAuditLog :one
SELECT count(*) FROM audit_log
WHERE (sqlc.narg('action')::text IS NULL OR action = sqlc.narg('action')::text)
  AND (sqlc.narg('target_type')::text IS NULL OR target_type = sqlc.narg('target_type')::text);
```

(`$2`-style positional and `sqlc.narg`/`sqlc.arg` named params cannot be mixed in one query in sqlc — convert the positional ones to `sqlc.arg('action')` etc. if codegen complains. Check the M8 `audit.sql` for the prevailing convention and match it.)

- [ ] **Step 4: Regenerate + read the emitted types.**

Run: `make sqlc-generate && go build ./...`
Read `internal/db/gen/moderation.sql.go` + `models.go`: confirm the enum param Go types (`BlobState`, `ModerationRule`, `ModerationAction`), the nullable wrappers (`decided_by`/`actor_id` → `pgtype.UUID`; `scheduled_tombstone_at` → `pgtype.Timestamptz`; `rule_ref`/`notes` → `pgtype.Text`; `payload` → `[]byte`), and that `IsBlocklisted`/`...Exists` returns `bool`. Adapt all later tasks to these exact names.

- [ ] **Step 5: codegen-check + commit.**

```bash
gofmt -w internal/db/gen/
git add internal/db/migrations/0008_moderation.sql internal/db/queries/moderation.sql internal/db/queries/audit.sql internal/db/gen docs/specs/DATA_MODEL.sql
git commit -m "feat(db): moderation schema (blocklist) + moderation/audit-log queries (sqlc)"
```

---

## Task 1: `internal/auditlog` — two-mode Writer

**Files:**
- Create: `internal/auditlog/writer.go`, `internal/auditlog/writer_test.go`

- [ ] **Step 1: Write failing tests** (`dbtest`): `WriteTx` inserts one `audit_log` row atomically and rolls back with its tx; `Write` (best-effort) inserts a row and **returns no error even when the insert fails** (point a `Writer` at a cancelled ctx and assert it does not panic / returns nothing).

```go
func TestWriteTxIsAtomicWithItsTx(t *testing.T) {
    if testing.Short() { t.Skip("integration") }
    ctx := context.Background()
    pool := dbtest.New(t, ctx)
    w := auditlog.NewWriter(gen.New(pool), slog.Default())

    tx, err := pool.Begin(ctx); require.NoError(t, err)
    require.NoError(t, w.WriteTx(ctx, tx, auditlog.Entry{
        Action: "dmca.quarantined", TargetType: "cid", TargetID: "bafyX",
        Payload: map[string]any{"reason": "test"}}))
    require.NoError(t, tx.Rollback(ctx)) // rolled back ⇒ no row

    n, err := gen.New(pool).CountAuditLog(ctx, gen.CountAuditLogParams{})
    require.NoError(t, err)
    require.EqualValues(t, 0, n)
}
```

- [ ] **Step 2: Run — expect FAIL** (`undefined: auditlog`). `go test ./internal/auditlog/`.

- [ ] **Step 3: Implement `writer.go`.**

```go
// Package auditlog writes the durable operator-action trail (audit_log). It has
// two modes: WriteTx (atomic, inside a moderation tx) and Write (best-effort,
// for post-commit backfilled call sites that must never be failed by audit I/O).
package auditlog

import (
    "context"
    "encoding/json"
    "log/slog"

    "github.com/google/uuid"
    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgtype"
    "github.com/nova-archive/nova/internal/db/gen"
)

type Entry struct {
    ActorID    *uuid.UUID
    Action     string
    TargetType string
    TargetID   string
    Payload    map[string]any
}

type Writer struct {
    q   *gen.Queries
    log *slog.Logger
}

func NewWriter(q *gen.Queries, log *slog.Logger) *Writer { return &Writer{q: q, log: log} }

func (w *Writer) params(e Entry) (gen.InsertAuditLogParams, error) {
    payload := []byte("{}")
    if e.Payload != nil {
        b, err := json.Marshal(e.Payload)
        if err != nil { return gen.InsertAuditLogParams{}, err }
        payload = b
    }
    actor := pgtype.UUID{}
    if e.ActorID != nil { actor = pgtype.UUID{Bytes: *e.ActorID, Valid: true} }
    return gen.InsertAuditLogParams{
        ActorID: actor, Action: e.Action, TargetType: e.TargetType,
        TargetID: e.TargetID, Payload: payload,
    }, nil
}

// WriteTx inserts atomically inside tx; an error rolls the caller's action back.
func (w *Writer) WriteTx(ctx context.Context, tx pgx.Tx, e Entry) error {
    p, err := w.params(e)
    if err != nil { return err }
    return w.q.WithTx(tx).InsertAuditLog(ctx, p)
}

// Write inserts best-effort on the pool; failures are logged, never returned.
func (w *Writer) Write(ctx context.Context, e Entry) {
    p, err := w.params(e)
    if err != nil { w.log.Warn("auditlog: marshal", "action", e.Action, "err", err); return }
    if err := w.q.InsertAuditLog(ctx, p); err != nil {
        w.log.Warn("auditlog: write", "action", e.Action, "err", err)
    }
}
```

(Adapt `InsertAuditLogParams` field names — esp. `ActorID` if the narg emits `pgtype.UUID` vs a pointer — to the Task-0 codegen.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(auditlog): two-mode audit_log writer (atomic + best-effort)`).

---

## Task 2: `internal/moderation` — errors, Service skeleton, Quarantine

**Files:**
- Create: `internal/moderation/errors.go`, `internal/moderation/service.go`, `internal/moderation/service_test.go`

- [ ] **Step 1: Write failing test** (`dbtest` + `blobfixture` + a fake `CascadeHook` + a real `auditlog.Writer`): quarantine an active encrypted blob with a seeded derivative → parent + derivative are `quarantined`, a `moderation_decisions` row exists with `scheduled_tombstone_at` set + `legal_hold=false`, a `('cid', cid)` revocation exists, the owner has 1 strike, and an `audit_log` `dmca.quarantined` row exists. With `legalHold=true` → DEK(s) `legal_hold=true` and `scheduled_tombstone_at IS NULL`.

```go
func TestQuarantineCascadesAndAudits(t *testing.T) {
    if testing.Short() { t.Skip("integration") }
    ctx := context.Background(); pool := dbtest.New(t, ctx)
    // seed: owner user, active encrypted parent blob + one derivative (blobfixture)
    svc := moderation.NewService(gen.New(pool), pool, fakeBackend{}, fakeCascade, auditlog.NewWriter(gen.New(pool), slog.Default()), slog.Default(), time.Now)
    require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
        CID: parentCID, Rule: "dmca", Reason: "notice", TombstoneAfter: 14*24*time.Hour, Actor: &opID}))
    // assert parent+derivative state, decision row, revocation, strike, audit row
}
```

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `errors.go` + the Service + Quarantine.**

```go
// errors.go
package moderation
import "errors"
var (
    ErrLegalHold      = errors.New("moderation: blocked by legal hold")
    ErrBlobNotFound   = errors.New("moderation: blob not found")
    ErrNotQuarantined = errors.New("moderation: blob is not quarantined")
)
```

```go
// service.go
package moderation

import (
    "context"; "errors"; "fmt"; "log/slog"; "time"
    "github.com/google/uuid"
    "github.com/ipfs/go-cid"
    "github.com/jackc/pgx/v5"; "github.com/jackc/pgx/v5/pgconn"; "github.com/jackc/pgx/v5/pgtype"; "github.com/jackc/pgx/v5/pgxpool"
    "github.com/nova-archive/nova/internal/auditlog"
    "github.com/nova-archive/nova/internal/db/gen"
    "github.com/nova-archive/nova/internal/ipfs"
)

// CascadeHook propagates a parent's new state to its derivatives. The coordinator
// wires this to product.OnDelete; moderation does not import the product package.
type CascadeHook func(ctx context.Context, tx pgx.Tx, parentCID, newState string) error

// Backend is the subset of ipfs.Backend tombstone needs (kept narrow for fakes).
type Backend interface{ Unpin(ctx context.Context, c cid.Cid) error }

type Service struct {
    q       *gen.Queries
    pool    *pgxpool.Pool
    backend Backend
    cascade CascadeHook
    audit   *auditlog.Writer
    log     *slog.Logger
    now     func() time.Time
}

func NewService(q *gen.Queries, pool *pgxpool.Pool, b Backend, c CascadeHook, a *auditlog.Writer, log *slog.Logger, now func() time.Time) *Service {
    if c == nil { c = func(context.Context, pgx.Tx, string, string) error { return nil } }
    if now == nil { now = time.Now }
    return &Service{q: q, pool: pool, backend: b, cascade: c, audit: a, log: log, now: now}
}

type QuarantineCmd struct {
    CID, Rule, RuleRef, Reason string
    TombstoneAfter             time.Duration
    LegalHold                  bool
    Actor                      *uuid.UUID
}

func (s *Service) Quarantine(ctx context.Context, cmd QuarantineCmd) error {
    tx, err := s.pool.Begin(ctx)
    if err != nil { return err }
    defer func() { _ = tx.Rollback(ctx) }()
    q := s.q.WithTx(tx)

    info, err := q.GetBlobForModeration(ctx, cmd.CID)
    if errors.Is(err, pgx.ErrNoRows) { return ErrBlobNotFound }
    if err != nil { return fmt.Errorf("moderation: get blob: %w", err) }

    var sched pgtype.Timestamptz
    if !cmd.LegalHold { sched = pgtype.Timestamptz{Time: s.now().Add(cmd.TombstoneAfter), Valid: true} }
    if _, err := q.InsertModerationDecision(ctx, gen.InsertModerationDecisionParams{
        Cid: cmd.CID, Rule: gen.ModerationRule(orDefault(cmd.Rule, "operator_manual")),
        RuleRef: text(cmd.RuleRef), Action: gen.ModerationAction("quarantine"),
        DecidedBy: uuidPg(cmd.Actor), ScheduledTombstoneAt: sched, LegalHold: cmd.LegalHold, Notes: text(cmd.Reason),
    }); err != nil { return fmt.Errorf("moderation: insert decision: %w", err) }

    if err := q.SetBlobState(ctx, gen.SetBlobStateParams{Cid: cmd.CID, State: gen.BlobState("quarantined")}); err != nil {
        return fmt.Errorf("moderation: set state: %w", err)
    }
    if err := s.cascade(ctx, tx, cmd.CID, "quarantined"); err != nil { return fmt.Errorf("moderation: cascade: %w", err) }

    if cmd.LegalHold {
        if err := q.SetDEKLegalHoldForBlobTree(ctx, gen.SetDEKLegalHoldForBlobTreeParams{Cid: cmd.CID, Hold: true}); err != nil {
            return fmt.Errorf("moderation: set legal hold: %w", err)
        }
    }
    if err := q.InsertRevocation(ctx, gen.InsertRevocationParams{Kind: "cid", Value: cmd.CID}); err != nil {
        return fmt.Errorf("moderation: insert revocation: %w", err)
    }
    if cmd.RuleRef != "" {
        if id, perr := uuid.Parse(cmd.RuleRef); perr == nil {
            if err := q.SetDMCACaseActioned(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
                return fmt.Errorf("moderation: action case: %w", err)
            }
        }
    }
    if info.OwnerID != "" {
        if oid, perr := uuid.Parse(info.OwnerID); perr == nil {
            if err := q.UpsertRepeatInfringer(ctx, pgtype.UUID{Bytes: oid, Valid: true}); err != nil {
                return fmt.Errorf("moderation: strike: %w", err)
            }
        }
    }
    action := "dmca.quarantined"; if cmd.LegalHold { action = "severe.quarantined" }
    if err := s.audit.WriteTx(ctx, tx, auditlog.Entry{ActorID: cmd.Actor, Action: action, TargetType: "cid", TargetID: cmd.CID,
        Payload: map[string]any{"reason": cmd.Reason, "case": cmd.RuleRef, "legal_hold": cmd.LegalHold}}); err != nil {
        return fmt.Errorf("moderation: audit: %w", err)
    }
    return tx.Commit(ctx)
}

// helpers: text(s) → pgtype.Text{Valid: s!=""}; uuidPg(*uuid)→pgtype.UUID; orDefault.
// isLegalHoldViolation reports the no_shred_under_legal_hold CHECK (used by Tombstone).
func isLegalHoldViolation(err error) bool {
    var pgErr *pgconn.PgError
    return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "no_shred_under_legal_hold"
}
```

(Adapt every `gen.*Params` field + the nullable wrappers to the Task-0 codegen. Reuse the M7 `InsertRevocation` query — confirm its param struct name.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(moderation): service + quarantine transaction (cascade, revocation, strike, audit)`).

---

## Task 3: Tombstone (shred + cascade + unpin) + the CHECK → ErrLegalHold mapping

**Files:**
- Modify: `internal/moderation/service.go`, `internal/moderation/service_test.go`

- [ ] **Step 1: Write failing tests:** (a) tombstone an active blob → `state='tombstoned'`, parent + derivative DEKs `state='shredded'` with zeroed `wrapped_key`, the originating quarantine decision's `scheduled_tombstone_at` is cleared, a revocation exists, and the fake backend recorded `Unpin(parent)` + `Unpin(derivative)`; (b) **tombstone a `legal_hold=true` blob returns `ErrLegalHold` and rolls back** — state still `quarantined`, DEK still `active`/unzeroed.

```go
func TestTombstoneRefusedUnderLegalHold(t *testing.T) {
    if testing.Short() { t.Skip("integration") }
    // seed quarantined blob with DEK legal_hold=true
    err := svc.Tombstone(ctx, moderation.TombstoneCmd{CID: cid, Reason: "x"})
    require.ErrorIs(t, err, moderation.ErrLegalHold)
    // assert state unchanged + DEK not shredded
}
```

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `Tombstone`.**

```go
type TombstoneCmd struct{ CID, Rule, RuleRef, Reason string; Actor *uuid.UUID }

var zeros72 = make([]byte, 72) // matches data_encryption_keys.wrapped_key width (M7 shred pattern)

func (s *Service) Tombstone(ctx context.Context, cmd TombstoneCmd) error {
    // collect derivative CIDs first (for the post-commit unpin loop)
    derivs, err := s.q.ListDerivativeCIDs(ctx, cmd.CID)
    if err != nil { return fmt.Errorf("moderation: list derivatives: %w", err) }

    tx, err := s.pool.Begin(ctx); if err != nil { return err }
    defer func() { _ = tx.Rollback(ctx) }()
    q := s.q.WithTx(tx)

    if _, err := q.InsertModerationDecision(ctx, gen.InsertModerationDecisionParams{
        Cid: cmd.CID, Rule: gen.ModerationRule(orDefault(cmd.Rule, "operator_manual")),
        RuleRef: text(cmd.RuleRef), Action: gen.ModerationAction("tombstone"),
        DecidedBy: uuidPg(cmd.Actor), LegalHold: false, Notes: text(cmd.Reason),
    }); err != nil { return fmt.Errorf("moderation: insert decision: %w", err) }

    if err := q.ClearScheduledTombstone(ctx, cmd.CID); err != nil { return fmt.Errorf("moderation: clear schedule: %w", err) }
    if err := q.SetBlobState(ctx, gen.SetBlobStateParams{Cid: cmd.CID, State: gen.BlobState("tombstoned")}); err != nil {
        return fmt.Errorf("moderation: set state: %w", err)
    }
    if err := s.cascade(ctx, tx, cmd.CID, "tombstoned"); err != nil { return fmt.Errorf("moderation: cascade: %w", err) }

    if err := q.ShredDEKsForBlobTree(ctx, gen.ShredDEKsForBlobTreeParams{Cid: cmd.CID, Zeros: zeros72}); err != nil {
        if isLegalHoldViolation(err) { return ErrLegalHold } // tx rolls back via defer
        return fmt.Errorf("moderation: shred: %w", err)
    }
    if err := q.InsertRevocation(ctx, gen.InsertRevocationParams{Kind: "cid", Value: cmd.CID}); err != nil {
        return fmt.Errorf("moderation: revocation: %w", err)
    }
    // ... SetDMCACaseActioned + UpsertRepeatInfringer (same as Quarantine, guarded) ...
    if err := s.audit.WriteTx(ctx, tx, auditlog.Entry{ActorID: cmd.Actor, Action: "dmca.tombstoned",
        TargetType: "cid", TargetID: cmd.CID, Payload: map[string]any{"reason": cmd.Reason, "case": cmd.RuleRef}}); err != nil {
        return fmt.Errorf("moderation: audit: %w", err)
    }
    if err := tx.Commit(ctx); err != nil { return err }

    // best-effort, idempotent — after commit; bytes are already inert.
    s.unpin(ctx, cmd.CID)
    for _, d := range derivs { s.unpin(ctx, d) }
    return nil
}

func (s *Service) unpin(ctx context.Context, cidStr string) {
    if s.backend == nil { return }
    c, err := cid.Decode(cidStr); if err != nil { s.log.Warn("moderation: bad cid for unpin", "cid", cidStr); return }
    if err := s.backend.Unpin(ctx, c); err != nil { s.log.Warn("moderation: unpin", "cid", cidStr, "err", err) }
}
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(moderation): tombstone + crypto-shred + derivative cascade + local unpin`).

---

## Task 4: ClearLegalHold + Restore + CounterNotice

**Files:**
- Modify: `internal/moderation/service.go`, `internal/moderation/service_test.go`

- [ ] **Step 1: Write failing tests:** `ClearLegalHold` flips DEK(s) `legal_hold=false`, sets the decision `scheduled_tombstone_at=now()`, audits `severe.legal_hold_cleared`, and a subsequent `Tombstone` now succeeds (the exit-criterion-3 chain); `Restore` sets `state='active'` (parent + derivative), clears `scheduled_tombstone_at`, audits `dmca.restored`, and **leaves the `('cid',cid)` revocation in place** (assert the row still exists); `CounterNotice` clears `scheduled_tombstone_at` and audits `dmca.counter_received` while the blob stays `quarantined`.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement.** Same `tx`/`WithTx`/`WriteTx`/commit envelope as above.

```go
func (s *Service) ClearLegalHold(ctx context.Context, cid string, caseRef, reason string, actor *uuid.UUID) error {
    // tx: SetDEKLegalHoldForBlobTree(cid, false); ClearModerationLegalHold(cid); WriteTx severe.legal_hold_cleared
}
func (s *Service) Restore(ctx context.Context, cid, reason string, actor *uuid.UUID) error {
    // tx: GetBlobForModeration → if state != quarantined return ErrNotQuarantined;
    //     SetBlobState(cid,'active'); cascade(...,'active'); ClearScheduledTombstone(cid);
    //     WriteTx dmca.restored.  NB: do NOT delete the revocation (design Point 4).
}
func (s *Service) CounterNotice(ctx context.Context, cid, notes string, actor *uuid.UUID) error {
    // tx: ClearScheduledTombstone(cid); (optional) InsertModerationDecision note row; WriteTx dmca.counter_received
}
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(moderation): clear-legal-hold, restore, counter-notice`).

---

## Task 5: Sweeper

**Files:**
- Create: `internal/moderation/sweeper.go`, `internal/moderation/sweeper_test.go`

- [ ] **Step 1: Write failing tests** (`dbtest`): with one overdue quarantine (non-legal-hold) and one legal-hold quarantine, `tick` tombstones only the former (state `tombstoned`, DEK shredded) and leaves the legal-hold one `quarantined`; a not-yet-due quarantine is untouched; a disabled sweeper never ticks.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `sweeper.go`.**

```go
type Sweeper struct {
    svc      *Service
    interval time.Duration
    enabled  bool
    log      *slog.Logger
}

func NewSweeper(svc *Service, interval time.Duration, enabled bool, log *slog.Logger) *Sweeper {
    if interval <= 0 { interval = time.Minute }
    return &Sweeper{svc: svc, interval: interval, enabled: enabled, log: log}
}

func (s *Sweeper) Run(ctx context.Context) {
    if !s.enabled { return }
    t := time.NewTicker(s.interval); defer t.Stop()
    for {
        select {
        case <-ctx.Done(): return
        case <-t.C: s.tick(ctx)
        }
    }
}

func (s *Sweeper) tick(ctx context.Context) {
    rows, err := s.svc.q.ListOverdueTombstones(ctx)
    if err != nil { s.log.Warn("moderation: sweep list", "err", err); return }
    for _, r := range rows {
        if err := s.svc.Tombstone(ctx, TombstoneCmd{CID: r.Cid, Rule: r.Rule, RuleRef: pgText(r.RuleRef), Reason: "scheduled tombstone"}); err != nil {
            s.log.Warn("moderation: sweep tombstone", "cid", r.Cid, "err", err) // legal-hold rows are filtered out; CHECK is the backstop
        }
    }
}

// Tick is exported for deterministic tests (no wall-clock wait).
func (s *Sweeper) Tick(ctx context.Context) { s.tick(ctx) }
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(moderation): in-process scheduled-tombstone sweep`).

---

## Task 6: blocklist enforcement in storage

**Files:**
- Modify: `pkg/coordinator/storage/errors.go`, `pkg/coordinator/storage/blob.go`, `pkg/coordinator/storage/put.go`, `internal/api/handlers/upload.go`, `internal/api/handlers/blob.go`

- [ ] **Step 1: Write failing tests:** `Resolve` of a blocklisted CID returns `ErrBlobBlocklisted` (add a row, then resolve an otherwise-active blob); `Put` of plaintext whose derived CID is pre-inserted into `blocklist` is refused with `ErrBlobBlocklisted` (use a `public_archival` write for a deterministic CID).

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement.**

`errors.go`: `ErrBlobBlocklisted = errors.New("storage: blob blocklisted")`.

`blob.go` `Resolve`, immediately after the `cid.Decode` guard (before `GetBlobCore`):
```go
blocked, err := s.q.IsBlocklisted(ctx, cidStr)
if err != nil { return nil, fmt.Errorf("storage: blocklist check: %w", err) }
if blocked { return nil, ErrBlobBlocklisted }
```

`put.go`: locate where the envelope CID is known and the DB commit has not happened (read the file; it is the `Put` write transaction). Before committing/pinning:
```go
blocked, err := s.q.IsBlocklisted(ctx, derivedCID)
if err != nil { return CommittedRef{}, fmt.Errorf("storage: blocklist check: %w", err) }
if blocked { return CommittedRef{}, ErrBlobBlocklisted }
```

`handlers/blob.go` `mapBytesError`: add `case errors.Is(err, storage.ErrBlobBlocklisted): return 451, "blocklisted", "content blocked"`. `handlers/upload.go`: map `storage.ErrBlobBlocklisted` → `451 "blocklisted"`.

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(storage): CID blocklist deny on read + import`).

---

## Task 7: public DMCA intake handler

**Files:**
- Create: `internal/api/handlers/dmca_intake.go`, `internal/api/handlers/dmca_intake_test.go`

- [ ] **Step 1: Write failing test:** POST a valid § 512(c)(3) body → `202` + `{case_id}` and a `dmca_cases(status='received')` row; a missing required field → `400`.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `dmca_intake.go`.**

```go
type DMCAIntakeHandler struct{ q *gen.Queries }
func NewDMCAIntakeHandler(q *gen.Queries) *DMCAIntakeHandler { return &DMCAIntakeHandler{q: q} }

type dmcaIntakeReq struct {
    ClaimantName   string `json:"claimant_name"`
    ClaimantEmail  string `json:"claimant_email"`
    SwornStatement string `json:"sworn_statement"`
    TargetCID      string `json:"target_cid"`
}

func (h *DMCAIntakeHandler) Submit(w http.ResponseWriter, r *http.Request) {
    rid := middleware.RequestIDFromContext(r.Context())
    var req dmcaIntakeReq
    if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
        httputil.WriteError(w, 400, "invalid_request", "bad json", rid); return
    }
    if req.ClaimantName == "" || req.ClaimantEmail == "" || req.SwornStatement == "" || req.TargetCID == "" {
        httputil.WriteError(w, 400, "invalid_request", "missing required field", rid); return
    }
    id, err := h.q.InsertDMCACase(ctx, gen.InsertDMCACaseParams{
        ClaimantName: req.ClaimantName, ClaimantEmail: req.ClaimantEmail,
        SwornStatement: req.SwornStatement, TargetCid: req.TargetCID})
    if err != nil { httputil.WriteError(w, 500, "internal", "internal", rid); return }
    w.Header().Set("Content-Type", "application/json"); w.WriteHeader(202)
    _ = json.NewEncoder(w).Encode(map[string]any{"case_id": uuidString(id)})
}
```

(Public route; relies on the existing rate-limit middleware. No action is taken.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(api): public POST /legal/dmca intake`).

---

## Task 8: moderation admin handler (actions + queue + cases + blocklist)

**Files:**
- Create: `internal/api/handlers/moderation_admin.go`, `internal/api/handlers/moderation_admin_test.go`

- [ ] **Step 1: Write failing tests** (handler-level with a real `Service` over `dbtest`): `POST /quarantine` quarantines + `200`; quarantine then `POST /takedown` tombstones; `POST /clear-legal-hold` clears; `POST /restore` reactivates; `GET /queue` paginates `moderation_decisions`; `GET /dmca` + `/dmca/{id}`; blocklist `GET`/`POST`/`DELETE`; `ErrLegalHold → 409`, bad body → `400`.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `moderation_admin.go`.** One handler struct over the `Service` + `*gen.Queries`; methods per route; map domain errors → status.

```go
type ModerationAdminHandler struct{ svc *moderation.Service; q *gen.Queries }
func NewModerationAdminHandler(svc *moderation.Service, q *gen.Queries) *ModerationAdminHandler { return &ModerationAdminHandler{svc, q} }

func actorFromCtx(r *http.Request) *uuid.UUID { /* bearer identity → uuid, nil if absent */ }

func mapModErr(err error) (int, string, string) {
    switch {
    case errors.Is(err, moderation.ErrLegalHold):      return 409, "legal_hold", "blocked by legal hold"
    case errors.Is(err, moderation.ErrNotQuarantined): return 409, "conflict", "blob is not quarantined"
    case errors.Is(err, moderation.ErrBlobNotFound):   return 404, "not_found", "blob not found"
    default:                                            return 500, "internal", "internal server error"
    }
}

func (h *ModerationAdminHandler) Quarantine(w http.ResponseWriter, r *http.Request) {
    // decode {cid, rule?, case_id?, reason, tombstone_after?, legal_hold?}; parse tombstone_after (default 14d);
    // svc.Quarantine(...); on err → mapModErr; else 200 {status:"quarantined"}
}
// Takedown, ClearLegalHold, Restore, CounterNotice — analogous; ClearLegalHold is mounted operator-only in server.go.
// Queue → ListModerationDecisions + CountModerationDecisions with httputil.ParsePage.
// DMCAList/DMCAGet → ListDMCACases / GetDMCACase. BlocklistList/Add/Remove → blocklist queries.
```

(Reuse `httputil.ParsePage`/`WriteError` and the M8 `{data, pagination}` response shape exactly. `tombstone_after` parses a Go duration string, e.g. `"14d"` → handle `d` suffix manually or accept hours.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(api): /api/v1/admin/moderation/* actions, queue, cases, blocklist`).

---

## Task 9: audit-log admin handler

**Files:**
- Create: `internal/api/handlers/audit_log_admin.go`, `internal/api/handlers/audit_log_admin_test.go`

- [ ] **Step 1: Write failing test:** seed a few `audit_log` rows; `GET /audit-log` returns `{data, pagination}` newest-first; `?action=` / `?target_type=` filter; pagination caps.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement** mirroring the M8 `AuditAdminHandler` (`audits_admin.go`): `ParsePage`, optional `action`/`target_type`/`actor_id` filters → `ListAuditLog`/`CountAuditLog` (narg), map rows to `{id, actor_id, action, target_type, target_id, payload, at}`.

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(api): paginated GET /api/v1/admin/audit-log`).

---

## Task 10: mount routes + ServerConfig + M7 audit backfill

**Files:**
- Modify: `internal/api/server.go`, `internal/api/handlers/signing_admin.go`

- [ ] **Step 1: Extend `ServerConfig`** with nil-able `ModerationAdmin *handlers.ModerationAdminHandler`, `DMCAIntake *handlers.DMCAIntakeHandler`, `AuditLogAdmin *handlers.AuditLogAdminHandler` (each `nil ⇒` unmounted), beside `SigningAdmin`/`AuditAdmin`.

- [ ] **Step 2: Mount.** Public `/legal/dmca` beside `/blob` (outside `/api/v1`): `if cfg.DMCAIntake != nil { r.Post("/legal/dmca", cfg.DMCAIntake.Submit) }`. Inside the `/api/v1/admin` group (already `RequireRole("operator","moderator")`):

```go
if cfg.ModerationAdmin != nil {
    r.Post("/moderation/quarantine", cfg.ModerationAdmin.Quarantine)
    r.Post("/moderation/takedown", cfg.ModerationAdmin.Takedown)
    r.With(bearer.RequireRole("operator")).Post("/moderation/clear-legal-hold", cfg.ModerationAdmin.ClearLegalHold)
    r.Post("/moderation/restore", cfg.ModerationAdmin.Restore)
    r.Post("/moderation/counter-notice", cfg.ModerationAdmin.CounterNotice)
    r.Get("/moderation/queue", cfg.ModerationAdmin.Queue)
    r.Get("/moderation/dmca", cfg.ModerationAdmin.DMCAList)
    r.Get("/moderation/dmca/{id}", cfg.ModerationAdmin.DMCAGet)
    r.Get("/moderation/blocklist", cfg.ModerationAdmin.BlocklistList)
    r.Post("/moderation/blocklist", cfg.ModerationAdmin.BlocklistAdd)
    r.Delete("/moderation/blocklist/{cid}", cfg.ModerationAdmin.BlocklistRemove)
}
if cfg.AuditLogAdmin != nil { r.Get("/audit-log", cfg.AuditLogAdmin.List) }
```

- [ ] **Step 3: M7 backfill.** Add a best-effort `*auditlog.Writer` field to `SigningAdminHandler` (nil-safe); in `RotateSigning`/`RevokeSignedURL`/`SignSignedURL`, after the action succeeds, `if h.audit != nil { h.audit.Write(ctx, auditlog.Entry{ActorID: actor, Action: "signing_key.rotated"|"signed_url.revoked"|"signed_url.signed", TargetType:"signing_key"|"cid", TargetID: ...}) }`. Update `NewSigningAdminHandler` to accept the writer (pass `nil` from any existing test call sites, or a real one from the coordinator).

- [ ] **Step 4: Build + unit.** `go build ./... && go test ./internal/api/... -short`. Expected PASS.

- [ ] **Step 5: gofmt; commit** (`feat(api): mount moderation + dmca-intake + audit-log routes; M7 audit backfill`).

---

## Task 11: extend the Maintainer to create-ahead audit_log partitions

**Files:**
- Modify: `internal/audit/integrity/retention.go`, `internal/audit/integrity/retention_test.go`

- [ ] **Step 1: Write failing test:** after `maintain`, `to_regclass('audit_log_YYYY_MM')` for next month is non-null; an `audit_log` insert dated next month succeeds; `audit_log` is **not** pruned (insert an old `pass`-irrelevant row and assert it survives — audit_log has no result column, so simply assert no audit_log rows are deleted).

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Refactor + extend.** Extract the month-derived create-ahead into a helper and call it for both tables:

```go
func ensureMonthlyPartitions(ctx context.Context, pool *pgxpool.Pool, parent string, now time.Time) error {
    base := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
    for i := 0; i < 3; i++ {
        start := base.AddDate(0, i, 0); end := start.AddDate(0, 1, 0)
        name := fmt.Sprintf("%s_%04d_%02d", parent, start.Year(), int(start.Month()))
        ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
            name, parent, start.Format("2006-01-02"), end.Format("2006-01-02"))
        if _, err := pool.Exec(ctx, ddl); err != nil { return fmt.Errorf("%s: %w", name, err) }
    }
    return nil
}
```

In `maintain`: `ensureMonthlyPartitions(ctx, m.pool, "integrity_audits", m.now())` and `ensureMonthlyPartitions(ctx, m.pool, "audit_log", m.now())`. Keep the existing `integrity_audits` prune/drop; **do not** prune `audit_log`.

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(audit): create-ahead audit_log partitions in the maintainer`).

---

## Task 12: coordinator + cmd wiring

**Files:**
- Modify: `pkg/coordinator/coordinator.go`, `pkg/coordinator/producthook.go`, `cmd/coordinator/main.go`

- [ ] **Step 1: Config.** Add to `coordinator.Config`:

```go
type ModerationConfig struct {
    SweepEnabled  bool          // default true
    SweepInterval time.Duration // default 1m; 0 ⇒ default
}
```

- [ ] **Step 2: Cascade adapter.** In `producthook.go`, add:

```go
func (h *productHook) OnDelete(ctx context.Context, tx pgx.Tx, parentCID, newState string) error {
    for _, p := range h.products { // Phase 1: the image product; OnDelete is the generic state cascade
        if err := p.OnDelete(ctx, tx, parentCID, newState); err != nil { return err }
    }
    return nil
}
```

- [ ] **Step 3: Build in `coordinator.New`** (when `pool != nil && backend != nil`): `auditW := auditlog.NewWriter(gen.New(pool), slog.Default())`; `modSvc := moderation.NewService(gen.New(pool), pool, backend, c.hook.OnDelete, auditW, slog.Default(), time.Now)` (pass `nil` cascade when no product hook); `c.modSweeper = moderation.NewSweeper(modSvc, cfg.Moderation.SweepInterval, cfg.Moderation.SweepEnabled, slog.Default())`; `sc.ModerationAdmin = handlers.NewModerationAdminHandler(modSvc, gen.New(pool))`; `sc.DMCAIntake = handlers.NewDMCAIntakeHandler(gen.New(pool))`; `sc.AuditLogAdmin = handlers.NewAuditLogAdminHandler(gen.New(pool))`. Pass `auditW` into `handlers.NewSigningAdminHandler(...)`. (The audit `Maintainer` already create-aheads `audit_log` after Task 11 — no extra wiring.)

- [ ] **Step 4: Start in `Run`** beside the others: `if c.modSweeper != nil { go c.modSweeper.Run(ctx) }`.

- [ ] **Step 5: cmd defaults.** In `cmd/coordinator/main.go`: `Moderation: coordinator.ModerationConfig{ SweepEnabled: os.Getenv("NOVA_MODERATION_SWEEP_ENABLED") != "false", SweepInterval: time.Minute }`; document `NOVA_MODERATION_SWEEP_ENABLED` in the header comment.

- [ ] **Step 6: Build + unit suite.** `go build ./... && go test ./internal/... ./pkg/... -short`. Expected PASS.

- [ ] **Step 7: gofmt; commit** (`feat(coordinator,cmd): wire moderation service + sweep + admin handlers + cascade`).

---

## Task 13: `novactl moderation` subcommand

**Files:**
- Modify: `cmd/novactl/main.go`

- [ ] **Step 1: Write failing test** (`cmd/novactl/main_test.go` style — argument parsing + the request shape against an `httptest` server returning canned JSON): `moderation quarantine <cid> --reason r --tombstone-after 14d` POSTs `/api/v1/admin/moderation/quarantine` with a bearer header.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement** a `cmdModeration(args []string)` dispatcher with `quarantine|takedown|clear-legal-hold|restore|list` sub-subcommands, each a `flag.FlagSet`, reusing `readCredentials`/`postJSON`/`getJSON`. Destructive verbs (`takedown`, `clear-legal-hold`) read a `y/N` confirmation from stdin unless `--no-confirm`. Wire into `main()`'s top-level switch and extend `usage()` + the package doc comment.

```go
case "moderation":
    if err := cmdModeration(args[1:]); err != nil { fmt.Fprintln(os.Stderr, "error:", err); os.Exit(1) }
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(novactl): moderation subcommand (quarantine/takedown/clear-legal-hold/restore/list)`).

---

## Task 14: integration test — end-to-end through nginx

**Files:**
- Create: `internal/integration/m9_moderation_test.go`

- [ ] **Step 1: Implement** the design § "Integration" scenario, reusing the M7/M8 nginx + Postgres testcontainer harness, the operator/moderator/uploader seed helpers, and `doJSONAuth`. Boot with a fast sweep interval (e.g. 1s) or drive `Sweeper.Tick` via a test seam. Cover:
  1. **Exit #1:** `POST /legal/dmca` → `GET /api/v1/admin/moderation/dmca` shows the case; `POST …/moderation/quarantine` (`case_id`, short `tombstone_after`) → `GET /blob/{cid}` = `451`; wait for the sweep → `GET /blob/{cid}` = `410`; assert DEK `shredded` (query) + the CID unpinned (offline backend records it); `GET …/audit-log` shows `dmca.quarantined` then `dmca.tombstoned`.
  2. **Exit #2:** `POST …/quarantine` `{legal_hold:true}` → `POST …/takedown` = `409 legal_hold`; DEK still `active`.
  3. **Exit #3:** `POST …/clear-legal-hold` (operator) → sweep → `410` + `shredded`.
  4. **Blocklist:** `POST …/blocklist {cid}` of a `public_archival` CID → re-import refused `451`; `GET /blob/{cid}` = `451`.
  5. **Authz:** operator + moderator `200` on queue/audit-log; no token `401`; `uploader` `403`; `clear-legal-hold` rejects `moderator` `403`.
  6. The extended Maintainer created next month's `audit_log` partition (assert `to_regclass`).

- [ ] **Step 2: Run** `go test ./internal/integration/ -run M9 -v`. Expected PASS (Docker required; `-short` skips).

- [ ] **Step 3: Commit** (`test(m9): end-to-end DMCA quarantine→tombstone, legal-hold refusal, blocklist, audit-log through nginx`).

---

## Task 15: documentation reconciliations

**Files:**
- Modify: `docs/specs/openapi.yaml`, `docs/legal/DMCA_PROCEDURE.md`, `docs/legal/SEVERE_CONTENT_PROCEDURE.md`, `docs/specs/INTEGRITY_AUDIT.md`, `docs/ROADMAP.md`

- [ ] **Step 1: openapi.yaml (#1)** — reconcile the blocklist from "perceptual-hash entry" (`:1091`) and the upload "hash blocklist scan" (`:556`) to **CID-based** (operator-curated; perceptual scan = Phase 3). Confirm/complete the request/response schemas + `401`/`403`/`451` for `POST /legal/dmca`, `/api/v1/admin/moderation/{quarantine,takedown,restore,counter-notice,clear-legal-hold,queue,blocklist,dmca,dmca/{id}}`, `DELETE …/blocklist/{cid}`, `GET /api/v1/admin/audit-log`.
- [ ] **Step 2: DMCA_PROCEDURE.md (#2)** — note Phase-1 unpin is local Kubo (broadcast = Phase 2); confirm the sweep SQL/cadence; note tombstone clears `scheduled_tombstone_at`.
- [ ] **Step 3: SEVERE_CONTENT_PROCEDURE.md (#3)** — confirm the implemented Phase-1 `--legal-hold` + operator-only `clear-legal-hold` match § "Phase 1 scope".
- [ ] **Step 4: INTEGRITY_AUDIT.md (#5)** — `derivative_state_consistent` now polices real cascades (drop the "inert until M9" framing).
- [ ] **Step 5: ROADMAP.md (#6)** — mark M9 status; link this design + plan; record deferrals (perceptual blocklist→Phase 3, NCMEC/SPA→Phase 4, auto-suspension, pinset reconciliation→Phase 5).
- [ ] **Step 6: Validate + commit.**

```bash
npx --yes @redocly/cli lint docs/specs/openapi.yaml 2>&1 | tail -5 || echo "(no redocly; skipping)"
git add docs/specs/openapi.yaml docs/legal/DMCA_PROCEDURE.md docs/legal/SEVERE_CONTENT_PROCEDURE.md docs/specs/INTEGRITY_AUDIT.md docs/ROADMAP.md
git commit -m "docs(m9): reconcile CID blocklist, local-unpin, severe-content + legal-hold, roadmap"
```

---

## Task 16: full suite + finish the branch

- [ ] **Step 1: Full unit + short suite.** `go build ./... && go test ./... -short`. Expected PASS. (`gofmt -l` may flag pre-existing files; only format files you touched.)
- [ ] **Step 2: M9 integration.** `go test ./internal/integration/ -run M9 -v`. Expected PASS.
- [ ] **Step 3: codegen-check.** `make sqlc-generate && git diff --exit-code internal/db/gen` (clean).
- [ ] **Step 4: Untagged build is the verified path.** `go build -o /tmp/nova-coordinator ./cmd/coordinator && echo OK`.
- [ ] **Step 5: Finish the branch.** Use `superpowers:finishing-a-development-branch` → fast-forward merge `m9-moderation` into `main` + annotated tag `m9-moderation` (local; no remote push), per the milestone workflow.

---

## Notes for the implementer

- **TDD discipline:** every package task is failing-test → run-FAIL → implement → run-PASS → commit. Do not batch.
- **Conform to `DMCA_PROCEDURE.md` exactly** (it is the normative transaction runbook): the quarantine/tombstone step order, the `audit_log` action vocabulary (`dmca.quarantined`/`dmca.tombstoned`/`dmca.restored`/`dmca.counter_received`/`severe.quarantined`/`severe.legal_hold_cleared`), and the sweep semantics.
- **The CHECK is the legal-hold floor.** Tombstone must let `ShredDEKsForBlobTree` raise the `no_shred_under_legal_hold` violation and map it to `ErrLegalHold`, rolling the whole tx back — never pre-check-and-skip in a way that could race. The DB is the enforcement boundary; this is exit criterion #2.
- **Audit atomicity vs best-effort (decided design):** moderation writes audit via `WriteTx` *inside* the action tx (the trail cannot diverge from the action). The M7 backfill uses best-effort `Write` *after* commit (never fails the action). **Do not audit `/auth/login`** — routine auth stays on M6's structured logs; this removes the login-latency tail.
- **In-process sweep, not the job queue:** the `Sweeper` re-reads `moderation_decisions` each tick; it must not enqueue `jobs.*`. Counter-notice/restore = clear `scheduled_tombstone_at`. Keep `internal/moderation` free of any `internal/jobs` import.
- **Tombstone clears the schedule.** Clearing `scheduled_tombstone_at` on the originating quarantine decision is what keeps the sweep's partial index (`moderation_decisions_scheduled_tombstone_idx`) and thus the per-minute sweep `O(pending)`. Do not add a redundant index — it would not help (completed rows keep `action='quarantine'`).
- **Restore leaves revocations (decided design):** `signed_url_revocations` has no issuer column, so a blind delete on restore could clobber a manual M7 operator revocation. Re-mint after restore; do not delete the `('cid', cid)` row.
- **Blocklist is a direct PK lookup, not a cache:** `blocklist.cid` is the PRIMARY KEY; call `q.IsBlocklisted` in `Resolve`/`Put`. No `RefreshEvery`, no in-memory set, no injected checker. (LRU is a documented future optimization only.)
- **Local unpin only (Phase 1):** tombstone calls `backend.Unpin` after commit, best-effort + idempotent. The federation broadcast is Phase 2. The unpin-after-commit orphan window is documented tech debt (design § Risks) — owner is a Phase-5 reconciliation pass; do not build it here.
- **Cascade via the product seam, DEK shred via the core:** derivative *state* flows through `product.OnDelete` (wired as `CascadeHook`); derivative *DEK* shred/legal-hold is the core's set-based `(b.cid=$1 OR b.parent_cid=$1)` update. Enumerate `ListDerivativeCIDs` only for the post-commit unpin loop.
- **Generated-type drift:** after Task 0, read `internal/db/gen/moderation.sql.go` + `models.go` — enum params are Go types (`BlobState`/`ModerationRule`/`ModerationAction`); nullable columns are `pgtype.{UUID,Timestamptz,Text}`; `payload` is `[]byte`; `IsBlocklisted` is `bool`. Adapt all call sites to the exact emitted names; reuse the M7 `InsertRevocation` param struct.
- **Role gating:** every admin action is `operator`+`moderator` (the group guard) **except** `clear-legal-hold`, which is `operator`-only (`SEVERE_CONTENT_PROCEDURE.md`); moderators "execute takedowns" per the `user_role` enum. The CLI confirmation prompt is the destructive-action safety, not a role wall.
- **Reuse, don't reinvent:** `httputil.ParsePage`/`Pagination`/`WriteError` (M8); the `{data, pagination}` response shape; `bearer.RequireRole`; the M3 read-path 451/410 mapping (already there — M9 only adds `ErrBlobBlocklisted`); `dbtest`/`blobfixture`/the nginx harness.
- **Test isolation:** seed `master_key_versions` before DEKs/blobs (FK); a `public_archival` write gives a deterministic CID for the blocklist re-import test; the offline embedded backend (or a fake recording `Unpin`) is the cheap path for the tombstone test.
```
