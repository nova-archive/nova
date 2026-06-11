# M11 Admin SPA Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship Nova's operator command center — a hermetic React + Vite Admin SPA (`web/admin/`) over login, blob list/view/soft-delete, moderation + DMCA, integrity-audit failures, key rotation, and a read-only jobs view — plus the small backend slice the SPA's named surfaces require: an owner soft-delete lifecycle (a neutral content-tombstone primitive extracted from M9), two admin list reads, and a coordinator static-serving seam.

**Architecture:** A new `internal/lifecycle` package owns the **neutral, irreversible content-tombstone primitive** (`TombstoneTree` = set `tombstoned` + cascade + crypto-shred DEK tree + revoke) extracted from `moderation.Tombstone`, plus owner **`SoftDelete`** (`active → soft_deleted`) and an in-process **`Sweeper`** that tombstones overdue soft-deletes after a grace window. `moderation.Tombstone` is refactored to call the shared primitive (behaviour unchanged). Owner routes `GET`/`DELETE /api/v1/blobs/{cid}` (documented but unmounted) get mounted; `GET /api/v1/admin/blobs` and read-only `GET /api/v1/admin/jobs` are added. The coordinator optionally serves `web/admin/dist` at `/admin/*` (CSP, SPA fallback) when `NOVA_ADMIN_DIST_DIR` is set. The SPA is a pure `/api/v1` client with two auth drivers (local issuer + external-OIDC PKCE) selected by `/auth/config`.

**Tech Stack:** Go, PostgreSQL (sqlc + inline pgx for jobs), chi, testcontainers + nginx harness; React 18 + Vite + TypeScript, React Router, TanStack Query, Radix UI, CSS Modules, `@fontsource/ibm-plex-*`, `openapi-typescript`, Vitest.

**Design:** `docs/superpowers/specs/phase1/2026-06-04-phase1-m11-admin-spa-design.md` is the source of truth; read its "The content-lifecycle primitive", "Coordinator static serving", and "The Admin SPA" sections before starting.

---

## File Structure

**Created:**
- `internal/db/migrations/0009_blob_soft_delete.sql` — `blobs.soft_deleted_at` + partial sweep index.
- `internal/lifecycle/tombstone.go` — `TombstoneTree`, `CascadeHook`/`Backend` types, `ErrLegalHold`/`ErrNotActive`.
- `internal/lifecycle/service.go` — `Service.SoftDelete`, `unpinTree`.
- `internal/lifecycle/sweeper.go` — overdue-soft-delete sweep (sibling to `moderation.Sweeper`).
- `internal/lifecycle/*_test.go`.
- `internal/jobs/admin.go` — read-only `AdminStore.List`/`Count` (inline pgx).
- `internal/jobs/admin_test.go`.
- `internal/api/handlers/blob_meta.go` — `GET` + `DELETE /api/v1/blobs/{cid}`.
- `internal/api/handlers/blobs_admin.go` — `GET /api/v1/admin/blobs`.
- `internal/api/handlers/jobs_admin.go` — `GET /api/v1/admin/jobs`.
- `internal/api/handlers/admin_spa.go` — `/admin/*` static (dist serving, CSP, SPA fallback).
- `internal/api/handlers/*_test.go`.
- `internal/integration/m11_admin_spa_test.go` — nginx-fronted e2e.
- `web/admin/*` — the SPA (scaffold, client, auth drivers, shell, screens, tokens, tests).

**Modified:**
- `internal/db/queries/blobs.sql` — `GetBlobMeta`, `MarkSoftDeleted`, `ListOverdueSoftDeletes`, `ListBlobs`, `CountBlobs`.
- `internal/db/gen/*` — regenerated.
- `internal/moderation/service.go` — `Tombstone` calls `lifecycle.TombstoneTree`; `CascadeHook`/`Backend` aliased to `lifecycle`.
- `internal/api/server.go` — mount owner blob routes + two admin lists + `/admin/*` static; new `ServerConfig` fields.
- `pkg/coordinator/coordinator.go` — build lifecycle Service+Sweeper + 4 handlers; `go sweeper.Run`; `AdminSPA`+`ContentLifecycle` config.
- `cmd/coordinator/main.go` — `NOVA_ADMIN_DIST_DIR`, `NOVA_SOFT_DELETE_GRACE_SECONDS`, `NOVA_LIFECYCLE_SWEEP_INTERVAL_MS`.
- `internal/config/types.go` — `AdminSPA` + `ContentLifecycle`.
- `Makefile`, `.github/workflows/ci.yml`, `package.json` — frontend lane.
- Docs: `openapi.yaml`, `DATA_MODEL.sql`, `THREAT_MODEL.md`, `OPERATOR_CHECKLIST.md`, `ROADMAP.md`, `PRODUCT_MODULE_INTERFACE.md`.

---

## Task 0: migration 0009 — `blobs.soft_deleted_at` + sweep index

**Files:**
- Create: `internal/db/migrations/0009_blob_soft_delete.sql`
- Modify: `docs/specs/DATA_MODEL.sql`

- [ ] **Step 1: Write the migration.** `blobs` (`0001_init.sql:286`) has no delete timestamp; the sweep needs one to age against. Follow the goose-style header of the existing migrations.

```sql
-- 0009_blob_soft_delete.sql
-- Owner soft-delete lifecycle (M11): records WHEN a blob entered soft_deleted so
-- the in-process lifecycle sweep can tombstone + crypto-shred it after the grace
-- window. The partial index serves the sweep's overdue claim.
ALTER TABLE blobs ADD COLUMN soft_deleted_at timestamptz;

CREATE INDEX blobs_soft_delete_sweep_idx
    ON blobs (soft_deleted_at)
    WHERE state = 'soft_deleted';
```

- [ ] **Step 2: Reconcile `docs/specs/DATA_MODEL.sql`.** Add the same column + index to the `blobs` definition with a comment: "set on owner soft-delete; the lifecycle sweep ages it against `NOVA_SOFT_DELETE_GRACE_SECONDS`, then tombstones via the shared `lifecycle.TombstoneTree` primitive (the same crypto-shred path as moderation)."

- [ ] **Step 3: Verify it applies.**

Run: `make migrate-up && make migrate-status` (or `make smoke`)
Expected: `0009` applied; `\d blobs` shows `soft_deleted_at` + `blobs_soft_delete_sweep_idx`.

```bash
git add internal/db/migrations/0009_blob_soft_delete.sql docs/specs/DATA_MODEL.sql
git commit -m "feat(db): blobs.soft_deleted_at + sweep index for owner soft-delete (m11)"
```

---

## Task 1: `blobs.sql` queries + regenerate

**Files:**
- Modify: `internal/db/queries/blobs.sql`, `internal/db/gen/*`

- [ ] **Step 1: Add the queries.** Append to `blobs.sql` (existing: `GetBlobCore`, `GetDEKByBlob`, `GetManifestSize`). Use `sqlc.narg` for optional filters (the `ListDMCACases` precedent in `moderation.sql`).

```sql
-- name: GetBlobMeta :one
-- State-agnostic metadata read for the owner/admin detail view (Resolve rejects
-- non-active states; this returns the row in ANY state for owner/operator).
SELECT cid, owner_id, parent_cid, derivative_preset, derivative_format,
       mime_type, byte_size, uploaded_at, soft_deleted_at, state, product
FROM blobs
WHERE cid = $1;

-- name: MarkSoftDeleted :execrows
-- active → soft_deleted; 0 rows ⇒ caller maps to 409 not_active (or 404 if absent).
UPDATE blobs
SET state = 'soft_deleted', soft_deleted_at = now()
WHERE cid = $1 AND state = 'active';

-- name: ListOverdueSoftDeletes :many
-- Claim overdue soft-deletes for the sweep, excluding legal-held trees (the
-- no_shred_under_legal_hold CHECK is the hard backstop). Mirror the legal-hold
-- filtering of ListOverdueTombstones (moderation.sql) for tree-wide holds.
SELECT b.cid
FROM blobs b
WHERE b.state = 'soft_deleted'
  AND b.soft_deleted_at < $1
  AND NOT EXISTS (
    SELECT 1 FROM data_encryption_keys d
    WHERE d.id = b.encryption_key_id AND d.legal_hold = true)
ORDER BY b.soft_deleted_at
LIMIT $2;

-- name: ListBlobs :many
-- Operator-wide listing for GET /api/v1/admin/blobs. Served by
-- blobs_product_state_idx / blobs_owner_state_idx / blobs_uploaded_at_idx.
SELECT cid, owner_id, parent_cid, mime_type, byte_size, uploaded_at, state, product
FROM blobs
WHERE (sqlc.narg(state)::blob_state     IS NULL OR state   = sqlc.narg(state))
  AND (sqlc.narg(product)::blob_product IS NULL OR product = sqlc.narg(product))
  AND (sqlc.narg(owner_id)::uuid        IS NULL OR owner_id = sqlc.narg(owner_id))
ORDER BY uploaded_at DESC
LIMIT $1 OFFSET $2;

-- name: CountBlobs :one
SELECT count(*)
FROM blobs
WHERE (sqlc.narg(state)::blob_state     IS NULL OR state   = sqlc.narg(state))
  AND (sqlc.narg(product)::blob_product IS NULL OR product = sqlc.narg(product))
  AND (sqlc.narg(owner_id)::uuid        IS NULL OR owner_id = sqlc.narg(owner_id));
```

(A `collection_id` filter via `EXISTS (SELECT 1 FROM collection_items …)` is an optional later add; M11 ships state/product/owner.)

- [ ] **Step 2: Regenerate + check.**

Run: `make sqlc-generate && make codegen-check && go build ./...`
Expected: `gen` gains `GetBlobMeta`/`GetBlobMetaRow`, `MarkSoftDeleted` (`int64`), `ListOverdueSoftDeletes`, `ListBlobs`/`ListBlobsParams`/`ListBlobsRow`, `CountBlobs`/`CountBlobsParams`. Clean diff otherwise.

```bash
git add internal/db/queries/blobs.sql internal/db/gen
git commit -m "feat(db): blob meta/list/soft-delete/sweep queries (sqlc) (m11)"
```

---

## Task 2: `internal/lifecycle` + refactor `moderation.Tombstone`

**Files:**
- Create: `internal/lifecycle/tombstone.go`, `service.go`, `sweeper.go`, `*_test.go`
- Modify: `internal/moderation/service.go`

- [ ] **Step 1: `tombstone.go` — the neutral primitive + shared types.** Move the destruction steps out of `moderation.Tombstone` (`service.go:190–207`). `zeros72` and the `isLegalHoldViolation` check move (or are duplicated minimally) here.

```go
package lifecycle

type CascadeHook func(ctx context.Context, tx pgx.Tx, parentCID, newState string) error
type Backend interface{ Unpin(ctx context.Context, c cid.Cid) error }

var zeros72 = make([]byte, 72) // data_encryption_keys.wrapped_key width (M7 shred pattern)

var (
    ErrLegalHold = errors.New("lifecycle: tombstone refused — DEK under legal hold")
    ErrNotActive = errors.New("lifecycle: blob is not active")
)

// TombstoneTree performs the irreversible, caller-agnostic destruction inside an
// existing tx: SetBlobState(tombstoned) → cascade → ShredDEKsForBlobTree → revoke.
// A legal-hold DEK raises no_shred_under_legal_hold (23514) → ErrLegalHold.
func TombstoneTree(ctx context.Context, q *gen.Queries, tx pgx.Tx, cascade CascadeHook, cidStr string) error {
    if err := q.SetBlobState(ctx, gen.SetBlobStateParams{Cid: cidStr, State: gen.BlobStateTombstoned}); err != nil {
        return fmt.Errorf("lifecycle: set state: %w", err)
    }
    if err := cascade(ctx, tx, cidStr, string(gen.BlobStateTombstoned)); err != nil {
        return fmt.Errorf("lifecycle: cascade: %w", err)
    }
    if err := q.ShredDEKsForBlobTree(ctx, gen.ShredDEKsForBlobTreeParams{Cid: cidStr, Zeros: zeros72}); err != nil {
        if isLegalHoldViolation(err) { return ErrLegalHold }
        return fmt.Errorf("lifecycle: shred: %w", err)
    }
    if err := q.InsertRevocation(ctx, gen.InsertRevocationParams{Kind: "cid", Value: cidStr}); err != nil {
        return fmt.Errorf("lifecycle: revoke: %w", err)
    }
    return nil
}
```

- [ ] **Step 2: `service.go` — owner soft-delete + unpin.**

```go
type Service struct {
    q       *gen.Queries
    pool    *pgxpool.Pool
    backend Backend
    cascade CascadeHook
    audit   *auditlog.Writer
    log     *slog.Logger
    now     func() time.Time
    grace   time.Duration
}

// SoftDelete flips active → soft_deleted (reversible during grace) and audits.
func (s *Service) SoftDelete(ctx context.Context, cidStr string, actor *uuid.UUID) error {
    tx, err := s.pool.Begin(ctx); if err != nil { return err }
    defer func() { _ = tx.Rollback(ctx) }()
    q := s.q.WithTx(tx)
    n, err := q.MarkSoftDeleted(ctx, cidStr)
    if err != nil { return fmt.Errorf("lifecycle: mark: %w", err) }
    if n == 0 { return ErrNotActive } // caller distinguishes 404 (absent) vs 409 via GetBlobMeta
    if err := s.cascade(ctx, tx, cidStr, string(gen.BlobStateSoftDeleted)); err != nil {
        return fmt.Errorf("lifecycle: cascade: %w", err)
    }
    if err := s.audit.WriteTx(ctx, tx, auditlog.Entry{
        ActorID: actor, Action: "blob.soft_deleted", TargetType: "cid", TargetID: cidStr,
        Payload: map[string]any{"cid": cidStr},
    }); err != nil { return fmt.Errorf("lifecycle: audit: %w", err) }
    return tx.Commit(ctx)
}
```

`unpinTree(ctx, parentCID, derivCIDs)` mirrors `moderation.Service.unpin` (best-effort, decode-and-Unpin, log on failure).

- [ ] **Step 3: `sweeper.go` — overdue tombstone sweep.** Sibling to `moderation/sweeper.go`; `NewSweeper(svc, interval, enabled, log)`, `Run(ctx)` ticker, `Tick(ctx)`:

```go
func (s *Sweeper) Tick(ctx context.Context) {
    cutoff := pgtype.Timestamptz{Time: s.svc.now().Add(-s.svc.grace), Valid: true}
    cids, err := s.svc.q.ListOverdueSoftDeletes(ctx, gen.ListOverdueSoftDeletesParams{SoftDeletedAt: cutoff, Limit: batch})
    // for each cid: derivs := ListDerivativeCIDs (pre-tx); tx{ TombstoneTree(tx, cascade, cid); audit "blob.tombstoned" (actor=nil, {cid, grace_seconds}) }; commit; unpinTree(cid, derivs)
    // ErrLegalHold should not occur (filtered) but is logged + skipped if it does.
}
```

- [ ] **Step 4: Refactor `moderation.Tombstone`.** Replace the four inline steps (`SetBlobState` + `cascade` + `ShredDEKsForBlobTree` + `InsertRevocation`, `service.go:190–207`) with `lifecycle.TombstoneTree(ctx, q, tx, s.cascade, cmd.CID)`; map `lifecycle.ErrLegalHold` → the existing `ErrLegalHold`. Keep `InsertModerationDecision`, `ClearScheduledTombstone`, `actionCaseRef`, `strikeOwner`, the `dmca.tombstoned` audit, and the post-commit unpin exactly as-is. Alias the shared types: `type CascadeHook = lifecycle.CascadeHook`, `type Backend = lifecycle.Backend` (so `NewService`'s signature is untouched). Remove the now-duplicated `zeros72`/`isLegalHoldViolation` from `moderation` if fully superseded, or keep `isLegalHoldViolation` for the mapping.

- [ ] **Step 5: Tests** (`dbtest` + `blobfixture`): `SoftDelete` (active→soft_deleted + soft_deleted_at set + derivative cascaded; non-active → `ErrNotActive`); `Sweeper.Tick` over a seeded overdue soft-delete (state `tombstoned`, DEK `wrapped_key==zeros72`/`state='shredded'`, unpin recorded); legal-hold tree skipped; `public_archival` (no-DEK) tombstones cleanly; `TombstoneTree` direct → `ErrLegalHold` rollback. **Run the existing moderation suite — it must pass unchanged.**

Run: `go test ./internal/lifecycle/... ./internal/moderation/... ./pkg/coordinator/...`
Expected: green; moderation behaviour identical.

```bash
git add internal/lifecycle internal/moderation/service.go
git commit -m "feat(lifecycle): neutral tombstone primitive + owner soft-delete + sweep; share with moderation (m11)"
```

---

## Task 3: `internal/jobs` admin read

**Files:**
- Create: `internal/jobs/admin.go`, `internal/jobs/admin_test.go`

- [ ] **Step 1: Read-only `AdminStore` (inline pgx).** The queue uses inline pgx (`queue.go`), so the admin read matches; served by `jobs_state_kind_idx (state, kind, created_at DESC)`.

```go
type AdminStore struct{ pool *pgxpool.Pool }
func NewAdminStore(pool *pgxpool.Pool) *AdminStore { return &AdminStore{pool} }

type JobRow struct {
    ID uuid.UUID; Kind, State string; Attempts, MaxAttempts int
    LastError *string; NotBefore, LeaseUntil *time.Time; CreatedAt, UpdatedAt time.Time
}
type Filter struct{ State, Kind string } // empty = no filter

func (s *AdminStore) List(ctx context.Context, f Filter, limit, offset int) ([]JobRow, error) {
    rows, err := s.pool.Query(ctx, `
        SELECT id, kind, state::text, attempts, max_attempts, last_error,
               not_before, lease_until, created_at, updated_at
        FROM jobs
        WHERE ($1 = '' OR state = $1::job_state) AND ($2 = '' OR kind = $2)
        ORDER BY created_at DESC
        LIMIT $3 OFFSET $4`, f.State, f.Kind, limit, offset)
    // scan → []JobRow
}
func (s *AdminStore) Count(ctx context.Context, f Filter) (int64, error) // same WHERE
```

- [ ] **Step 2: Tests** — seed jobs across states/kinds; assert filter, newest-first, pagination, `last_error` surfaced.

Run: `go test ./internal/jobs/...`

```bash
git add internal/jobs/admin.go internal/jobs/admin_test.go
git commit -m "feat(jobs): read-only admin List/Count for the jobs view (m11)"
```

---

## Task 4: handlers — owner blob routes + two admin lists

**Files:**
- Create: `internal/api/handlers/blob_meta.go`, `blobs_admin.go`, `jobs_admin.go` (+ tests)

- [ ] **Step 1: `blob_meta.go` — `GET` + `DELETE /api/v1/blobs/{cid}`.** Identity from context (the bearer middleware). Authz: `GET` allowed for the blob's `owner_id` or operator/moderator; `DELETE` for owner or operator.

```go
type BlobMetaHandler struct { q *gen.Queries; life *lifecycle.Service }

func (h *BlobMetaHandler) Get(w, r) {
    meta, err := h.q.GetBlobMeta(ctx, cid)            // pgx.ErrNoRows → 404 not_found
    if !ownerOrRole(r, meta.OwnerID, "operator","moderator") { 403 forbidden }
    writeJSON(200, blobJSON(meta))                    // matches openapi Blob schema
}
func (h *BlobMetaHandler) Delete(w, r) {
    meta, err := h.q.GetBlobMeta(ctx, cid)            // 404 if absent
    if !ownerOrRole(r, meta.OwnerID, "operator") { 403 }
    switch h.life.SoftDelete(ctx, cid, actorFromCtx(r)) {
    case nil:                204
    case lifecycle.ErrNotActive: 409 not_active
    default:                 500
    }
}
```

- [ ] **Step 2: `blobs_admin.go` — `GET /api/v1/admin/blobs`.** Parse `state`/`product`/`owner_id`/`page`/`per_page`; call `ListBlobs` + `CountBlobs`; return `PaginatedBlobs` (the `PaginatedModerationDecisions` envelope shape). Validate enum filters → 400 `invalid_request`.

- [ ] **Step 3: `jobs_admin.go` — `GET /api/v1/admin/jobs`.** Parse `state`/`kind`/`page`/`per_page`; call `AdminStore.List`/`Count`; return `PaginatedJobs`.

- [ ] **Step 4: Tests** — `blob_meta`: owner 200 / non-owner 403 / operator 200; `DELETE` owner 204, moderator 403, non-active 409, unknown 404. `blobs_admin`/`jobs_admin`: 401 no token / 403 wrong role (handled by the group guard in Task 6, but unit-test the handler + filter parsing/pagination shape).

Run: `go test ./internal/api/handlers/...`

```bash
git add internal/api/handlers/blob_meta.go internal/api/handlers/blobs_admin.go internal/api/handlers/jobs_admin.go internal/api/handlers/*_test.go
git commit -m "feat(api): blob meta+soft-delete, admin blobs list, admin jobs list handlers (m11)"
```

---

## Task 5: handler — `/admin/*` static SPA serving

**Files:**
- Create: `internal/api/handlers/admin_spa.go` (+ test)

- [ ] **Step 1: `AdminSPAHandler`.** Built only when `NOVA_ADMIN_DIST_DIR` is non-empty (nil otherwise). Serves the dist dir with SPA fallback + strict CSP.

```go
type AdminSPAHandler struct { dist string; fs http.Handler }
func NewAdminSPA(dist string) *AdminSPAHandler {
    if dist == "" { return nil }
    return &AdminSPAHandler{dist: dist, fs: http.FileServer(http.Dir(dist))}
}
// Serve: set CSP + nosniff; if the requested file exists under dist → serve it
// (immutable cache for /assets/*); else serve index.html (no-store) for client routing.
func (h *AdminSPAHandler) Serve(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Security-Policy",
        "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
        "font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
    w.Header().Set("X-Content-Type-Options", "nosniff")
    // path-safe join; existing asset → FileServer (+ immutable for /assets/); else index.html (no-store)
}
```

- [ ] **Step 2: Tests** — existing asset served with immutable cache; unknown `/admin/x` → `index.html` + no-store; CSP + nosniff present; path traversal (`/admin/../etc`) rejected; `NewAdminSPA("")` → nil (route 404 wiring covered in Task 6).

Run: `go test ./internal/api/handlers/...`

```bash
git add internal/api/handlers/admin_spa.go internal/api/handlers/admin_spa_test.go
git commit -m "feat(api): coordinator-served admin SPA (/admin/*) with CSP + SPA fallback (m11)"
```

---

## Task 6: mount routes + ServerConfig + coordinator/cmd wiring

**Files:**
- Modify: `internal/api/server.go`, `pkg/coordinator/coordinator.go`, `cmd/coordinator/main.go`, `internal/config/types.go`

- [ ] **Step 1: `ServerConfig` + routes (`server.go`).** Add nil-able fields `BlobMeta *handlers.BlobMetaHandler`, `BlobsAdmin *handlers.BlobsAdminHandler`, `JobsAdmin *handlers.JobsAdminHandler`, `AdminSPA *handlers.AdminSPAHandler`.
  - Owner blob routes inside the identity-aware group (beside `/users/me`, `:129`): `r.With(bearer.RequireAuthenticated).Get("/blobs/{cid}", cfg.BlobMeta.Get)` and `.Delete("/blobs/{cid}", cfg.BlobMeta.Delete)` (in-handler owner/role authz).
  - Admin lists inside the `/admin` group (`:139`): `r.Get("/blobs", cfg.BlobsAdmin.List)`, `r.Get("/jobs", cfg.JobsAdmin.List)` (nil ⇒ `adminNotFound`).
  - Static: at the **top level** (not under `/api/v1`), `if cfg.AdminSPA != nil { r.Handle("/admin", ...); r.Handle("/admin/*", http.HandlerFunc(cfg.AdminSPA.Serve)) }`. Distinct from `/api/v1/admin`.

- [ ] **Step 2: `internal/config/types.go`.**

```go
type AdminSPA struct { DistDir string `yaml:"dist_dir"` }
type ContentLifecycle struct {
    SoftDeleteGrace time.Duration `yaml:"soft_delete_grace"` // default 24h
    SweepInterval   time.Duration `yaml:"sweep_interval"`    // default 1m
}
```
Add both to `coordinator.Config`.

- [ ] **Step 3: `coordinator.go` wiring.** When `pool+backend+ks` present: build `lifecycle.NewService(gen.New(pool), pool, backend, cascade, auditW, slog, time.Now, cfg.ContentLifecycle.SoftDeleteGrace)` (reuse the same `cascade` wired to `product.OnDelete` that moderation uses), `lifecycle.NewSweeper(svc, SweepInterval, enabled, log)`, and the handlers `NewBlobMeta`, `NewBlobsAdmin(gen.New(pool))`, `NewJobsAdmin(jobs.NewAdminStore(pool))`, `NewAdminSPA(cfg.AdminSPA.DistDir)`. Set the `ServerConfig` fields. In `Run`, `go sweeper.Run(ctx)` beside `modSweeper`.

- [ ] **Step 4: `cmd/coordinator/main.go` env knobs.** `NOVA_ADMIN_DIST_DIR` (→ `AdminSPA.DistDir`, empty disables), `NOVA_SOFT_DELETE_GRACE_SECONDS` (default 86400), `NOVA_LIFECYCLE_SWEEP_INTERVAL_MS` (default 60000), each falling back when unset/zero. Update the env-table doc comment.

- [ ] **Step 5: Build + coordinator tests.**

Run: `go build ./... && go test ./internal/api/... ./pkg/coordinator/...`
Expected: routes mount; nil handlers ⇒ 404; sweep starts.

```bash
git add internal/api/server.go pkg/coordinator/coordinator.go cmd/coordinator/main.go internal/config/types.go
git commit -m "feat(coordinator,api,cmd): wire lifecycle sweep + blob/jobs/admin-SPA handlers + env knobs (m11)"
```

---

## Task 7: openapi additions + codegen

**Files:**
- Modify: `docs/specs/openapi.yaml`, `internal/api/codegen/*` (if generated)

- [ ] **Step 1: Mount + add paths.** Confirm `GET`/`DELETE /api/v1/blobs/{cid}` (already documented as `getBlobMeta`/`deleteBlob`) match the handlers; add `GET /api/v1/admin/blobs` (`listAdminBlobs` → `PaginatedBlobs`) and `GET /api/v1/admin/jobs` (`listAdminJobs` → `PaginatedJobs`) with the `state`/`product`/`owner_id`/`kind`/`page`/`per_page` query params, mirroring `listModerationQueue`. Add `PaginatedBlobs`/`PaginatedJobs` + `JobSummary` schemas (Blob already exists). Note the non-API `/admin/*` static surface in the description.

- [ ] **Step 2: Codegen drift check.**

Run: `make codegen-check` (oapi-codegen) `&& go build ./...`
Expected: clean (server uses hand-written handlers; ensure the spec validates and any generated types match).

```bash
git add docs/specs/openapi.yaml internal/api/codegen
git commit -m "docs(openapi): blobs/{cid} GET+DELETE, admin/blobs, admin/jobs (m11)"
```

---

## Task 8: SPA scaffold — build, client, auth drivers, shell, CI lane

**Files:**
- Create: `web/admin/{package.json,vite.config.ts,tsconfig.json,index.html,.eslintrc,vitest.config.ts}`, `web/admin/src/{main.tsx,App.tsx,tokens.css,api/*,auth/*,components/*}`
- Modify: `package.json` (root workspace scripts), `Makefile`, `.github/workflows/ci.yml`

- [ ] **Step 1: Scaffold (hermetic).** Vite + React-TS. `vite.config.ts`: `base: '/admin/'`, `build.outDir: 'dist'`, no external CDN. Add deps (pinned, bundled): `react`, `react-dom`, `react-router-dom`, `@tanstack/react-query`, `@radix-ui/react-dialog` + `-dropdown-menu` + `-toast`, `@fontsource/ibm-plex-sans|serif|mono`, and dev: `typescript`, `vite`, `vitest`, `@testing-library/react`, `eslint`, `openapi-typescript`. Import the IBM Plex `@fontsource` latin CSS in `main.tsx` (self-hosted; no Google Fonts).

- [ ] **Step 2: `tokens.css` from the brand.** Port the brand custom properties (`docs/design/Nova Brand _standalone_.html`): `--paper/-2`, `--card/-deep`, `--ink/-2/-mute/-faint`, `--nova/-deep`, `--ok`, `--slate/-2`, `--rule/-soft`, and `--sans/--serif/--mono` with IBM Plex first and the cross-OS system fallback stack the brand encodes. CSS Modules consume these vars; no utility framework.

- [ ] **Step 3: Typed API client.** `npm run gen:api` → `openapi-typescript docs/specs/openapi.yaml -o src/api/schema.ts`; a thin `authedFetch` wrapper injecting the bearer + handling 401 (refresh or re-login). TanStack Query hooks per resource (`useBlobs`, `useBlob`, `useSoftDelete`, `useModerationQueue`, `useDmcaCases`, `useIntegrityFailures`, `useRotationStatus`, `useJobs`).

- [ ] **Step 4: Auth drivers behind one provider.** `src/auth/`:
  - `AuthProvider` calls `GET /api/v1/auth/config` on boot, picks a driver, exposes `{user, login, logout, authedFetch}`; loads `GET /api/v1/users/me` for role.
  - `localDriver.ts`: `POST /auth/login` → access+refresh in `localStorage`; a refresh timer (`POST /auth/refresh`, rotation) fires before access expiry; `POST /auth/logout`.
  - `oidcPkceDriver.ts`: `mode=external` → generate `code_verifier`/`code_challenge` (Web Crypto S256), redirect to `issuer_url` authorize with `client_id`/`scopes`/`redirect_uri=/admin/callback`; the callback route exchanges the code at the IdP token endpoint, stores + silently renews. PKCE is client↔IdP only.

- [ ] **Step 5: App shell.** `App.tsx` with React Router under `/admin`, a `RequireAuth` wrapper, sidebar nav + header (current user + logout), and a Radix `Toast` provider. Role-aware nav (hide operator-only entries for moderators).

- [ ] **Step 6: CI lane + Makefile + workspace.**
  - Root `package.json`: ensure `web/admin` workspace + scripts; `Makefile`:
    ```make
    admin-build:  ; npm --prefix web/admin ci && npm --prefix web/admin run build
    admin-lint:   ; npm --prefix web/admin run lint
    admin-test:   ; npm --prefix web/admin run test -- --run
    hermetic-spa: ; ./scripts/hermetic-spa.sh web/admin/dist
    ```
  - `scripts/hermetic-spa.sh`: fail if the built bundle references any external origin:
    ```bash
    #!/usr/bin/env bash
    set -euo pipefail
    dist="${1:?dist dir}"
    if grep -rEoI "https?://[A-Za-z0-9.-]+" "$dist" | grep -vE "(w3\.org|localhost|127\.0\.0\.1)" ; then
      echo "hermetic-spa: external origin in admin bundle" >&2; exit 1
    fi
    echo "hermetic-spa: clean"
    ```
  - `.github/workflows/ci.yml`: a `web-admin` job (Node 22) running `admin-build`/`admin-lint`/`admin-test`/`hermetic-spa`.

Run: `make admin-build admin-lint admin-test hermetic-spa`
Expected: builds; lint/tests pass; hermetic gate clean.

```bash
git add web/admin scripts/hermetic-spa.sh Makefile .github/workflows/ci.yml package.json
git commit -m "feat(admin): SPA scaffold — hermetic build, typed client, auth drivers, shell, CI lane (m11)"
```

---

## Task 9: SPA screens

**Files:**
- Create: `web/admin/src/screens/*` (+ `*.test.tsx`)

- [ ] **Step 1: Login** — local form OR "Continue with <IdP>" per `/auth/config`; error states; redirect post-auth.
- [ ] **Step 2: Blobs** — `BlobsList` (table: cid, owner, product, state, size, uploaded; filters state/product/owner; pagination) → row → `BlobDetail` (metadata + image preview via `/i/{cid}` or `/blob/{cid}`; **soft-delete** behind a Radix `Dialog` confirm; operator/owner only).
- [ ] **Step 3: Moderation** — queue + actions (quarantine/takedown/restore/counter-notice via existing endpoints), DMCA cases list/detail, CID blocklist (list/add/remove). **No clear-legal-hold UI** (Phase 4).
- [ ] **Step 4: Audits** — integrity-audit failures (`result=fail`), filter by `audit_kind`.
- [ ] **Step 5: Keys** — master rotate (confirm modal) + `rotation-status` poll with progress; signing rotate (operator-only). Hide operator-only controls for moderators.
- [ ] **Step 6: Jobs** — read-only table (state/kind filters, `attempts`, `last_error`). No action buttons.
- [ ] **Step 7: Vitest** — driver selection + local refresh + PKCE verifier/callback (mock IdP); a representative screen's render/filter/pagination; role-gated control visibility; soft-delete confirm.

Run: `make admin-test admin-build hermetic-spa`
Expected: green; bundle hermetic.

```bash
git add web/admin/src
git commit -m "feat(admin): operator screens — blobs, moderation, audits, keys, jobs (m11)"
```

---

## Task 10: integration test (nginx-fronted, end-to-end)

**Files:**
- Create: `internal/integration/m11_admin_spa_test.go`

- [ ] **Step 1: Harness.** Build `web/admin/dist` (or skip with a clear message if Node is absent in the unit lane — the CI integration job builds it first). Boot the coordinator with `AdminDistDir=<dist>` + nginx (existing `location /admin` proxy) via `startCoordinatorWithNginxCfg`. Seed users (`seedAuthUser` operator/moderator/uploader) + blobs (`blobfixture.Seed`, incl. a derivative + a legal-hold blob via M9).

- [ ] **Step 2: Assert the exit criteria** (`m6Login`, `doJSONAuth`):
  1. **Serving:** `GET /admin/` → 200 `index.html` + strict CSP; `GET /admin/blobs` (deep link) → `index.html`; bundle has no external origin (scan `dist`); a second boot with `AdminDistDir=""` → `/admin/` 404.
  2. **Lifecycle:** local login → `GET /api/v1/admin/blobs` (seeded) → `GET /api/v1/blobs/{cid}` → `DELETE` → public `GET /blob/{cid}` 410; with a short grace, run the sweep (`Tick`) → still 410, DEK `state='shredded'`; `audit_log` has `blob.soft_deleted` then `blob.tombstoned`.
  3. **Lists:** `GET /api/v1/admin/jobs` read-only (+ `last_error` surfaced); `GET /api/v1/admin/blobs` filters by state/product.
  4. **External-OIDC:** boot a variant with an external verifier + stub IdP; `/auth/config` advertises it; a stub-IdP token is accepted; local `/auth/login` → `404 external_oidc_active`.
  5. **Authz:** moderator → 403 on `DELETE /blobs/{cid}` and rotate-master; no token → 401.

Run: `go test ./internal/integration/ -run M11`
Expected: all assertions pass.

```bash
git add internal/integration/m11_admin_spa_test.go
git commit -m "test(m11): nginx-fronted e2e — serving, soft-delete lifecycle, lists, OIDC, authz (m11)"
```

---

## Task 11: doc reconciliations + roadmap

**Files:**
- Modify: `docs/THREAT_MODEL.md`, `docs/legal/OPERATOR_CHECKLIST.md`, `docs/ROADMAP.md`, `docs/specs/PRODUCT_MODULE_INTERFACE.md`

- [ ] **Step 1: THREAT_MODEL** — hermetic-admin-bundle commitment (no third-party runtime assets; CI-enforced) + the `/admin` origin note (single-vhost in M11; two-vhost split M13).
- [ ] **Step 2: OPERATOR_CHECKLIST** — admin-SPA runbook: build + `NOVA_ADMIN_DIST_DIR`; soft-delete grace knob + "tombstone+shred is irreversible after grace"; login modes (local vs external OIDC via `/auth/config`).
- [ ] **Step 3: ROADMAP** — M11 status, link this design + plan, record the deferrals (jobs retry, blob PATCH / images / collections / perceptual search, clear-legal-hold UI → Phase 4, widget → M12, wizard + nginx templating → M13). (Tag `m11-admin-spa` recorded on completion.)
- [ ] **Step 4: PRODUCT_MODULE_INTERFACE** — one line: owner deletion is now a second caller of the `OnDelete` cascade (no interface change).

- [ ] **Step 5: Full verification before merge.**

Run: `make test && make codegen-check && make admin-build admin-lint admin-test hermetic-spa`
Expected: all green; hermetic gate clean.

```bash
git add docs/
git commit -m "docs(m11): reconcile threat-model, operator runbook, roadmap, product-module-interface (m11)"
```

---

## Self-review notes (for the executor)

- **Soft-delete is the long pole.** Land Tasks 0–6 (the lifecycle backend) and prove the sweep shreds after grace *and the moderation suite still passes* before investing in SPA polish (Tasks 8–9). The React app is mechanical; the crypto-shred reuse is the risk.
- **One crypto-shred.** `ShredDEKsForBlobTree`/`zeros72` must live in exactly one place after Task 2 — `lifecycle.TombstoneTree`. Do not leave a second copy in `moderation`.
- **Audit vocabulary is the semantic boundary.** Owner lifecycle writes `blob.soft_deleted`/`blob.tombstoned`; moderation keeps `dmca.*`/`severe.*`. Never let owner deletion write a `moderation_decisions` row.
- **`/admin` (static) vs `/api/v1/admin` (API).** Mount the static handler at the top level; it must never shadow the API group, and the SPA fallback must not return `index.html` for `/api/...` paths.
- **Hermetic is enforced, not aspirational.** Self-host fonts; the `hermetic-spa` gate fails the build on any external origin. If IBM Plex self-hosting fights CSP/licensing/reproducibility, fall back to the system stack — local-only is the invariant.
- **PKCE is client-side.** The coordinator gains no auth surface; the local driver is the default and must work standalone. Cover the PKCE flow with a stub IdP (unit) + an operator human-action check (real IdP).
- **Roadmap deferrals are real scope walls.** No jobs retry, no blob PATCH/images/collections, no clear-legal-hold UI, no widget/wizard. Keep M11 to the named surfaces.
- **Toolchain:** gofmt only the Go files M11 touches; `golangci-lint`/`eslint` are CI-side; `sqlc` via `make sqlc-generate`; Conventional Commits; no remote push (local merge + `m11-admin-spa` tag per the milestone workflow).
```
