# Phase 1 M11 — Admin SPA Design

## Purpose and scope

M11 is the eleventh Phase-1 milestone and the **first human-facing surface** (M11–M13 add
the friendly surfaces; M14 polishes). It delivers Nova's **operator command center**: a
hermetic React + Vite single-page app under `web/admin/` that an operator uses to log in,
browse and soft-delete blobs, work the moderation queue and DMCA cases, review integrity-audit
failures, drive key rotation, and inspect the job queue. `docs/ROADMAP.md` M11 row and the
master breakdown (`docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md`
§ "Walking-skeleton milestone breakdown" → M11) commit exactly that surface.

**M11 is not a pure-frontend milestone.** Reading the committed router (`internal/api/server.go`)
against `docs/specs/openapi.yaml` reveals real **spec-vs-router drift**: three of the SPA's
named surfaces have no backend mounted today, and one has no backend at all.

- **Owner soft-delete has no write path.** `DELETE /api/v1/blobs/{cid}` is fully documented in
  the openapi (`deleteBlob`, soft-delete semantics) but **is not mounted**; M6 deferred owner
  blob CRUD to "a later REST milestone / M11". `internal/db/queries/writes.sql` holds only
  inserts; `pkg/coordinator/storage/blob.go` *recognizes* `soft_deleted`/`tombstoned` on the
  read path (`:107–110`, returns `ErrBlobSoftDeleted`/`ErrBlobTombstoned`) but nothing ever
  *sets* those states. The only crypto-shred/unpin machinery is moderation's
  (`internal/moderation/service.go` `Tombstone`, `:159`).
- **No blob list.** `internal/db/queries/blobs.sql` has only `GetBlobCore`/`GetDEKByBlob`/
  `GetManifestSize` — there is no operator-wide listing for the "blob list" screen.
- **No jobs surface.** `internal/jobs` (queue/worker, inline pgx) has no admin read; the
  `jobs_state_kind_idx ON jobs (state, kind, created_at DESC)` index (`0002_jobs.sql:45`) was
  created "for admin introspection" but nothing reads it yet.

So M11 = **a focused backend slice (a neutral content-lifecycle primitive + owner soft-delete +
two admin list reads) + the SPA + a coordinator static-serving seam + a frontend CI lane.** The
backend slice is small but load-bearing; the soft-delete lifecycle (not the React app) is the
milestone's long pole.

### In scope

- **`internal/lifecycle`** — a new package owning the **neutral, irreversible content-tombstone
  primitive** extracted from M9 plus the **owner soft-delete** lifecycle. `TombstoneTree(ctx, q,
  tx, cascade, cid)` performs the destruction steps that are *not* moderation-specific (set
  `blobs.state='tombstoned'`, cascade to derivatives, `ShredDEKsForBlobTree` with the
  `no_shred_under_legal_hold` CHECK → `ErrLegalHold`, insert a `('cid', cid)` revocation), plus a
  post-commit best-effort unpin of the parent + derivatives. A `Service.SoftDelete(ctx, cid,
  actor)` flips `active → soft_deleted` (records `soft_deleted_at`, cascades), and a `Sweeper`
  (sibling to the M9 `moderation.Sweeper`) tombstones overdue soft-deletes through the shared
  primitive after the grace window. `internal/moderation`'s `Tombstone` is **refactored to call
  `lifecycle.TombstoneTree`** for the neutral steps, keeping its own decision/case/strike/audit
  bookkeeping — behaviour unchanged, machinery shared, semantics distinct.
- **Migration `0009_blob_soft_delete.sql`** — `blobs.soft_deleted_at timestamptz` (NULL until
  soft-deleted) + a partial sweep index `WHERE state = 'soft_deleted'`. (M11's only schema change;
  the rest of the surface is already in `0001_init.sql`.)
- **Owner blob routes** — mount the documented `GET /api/v1/blobs/{cid}` (authenticated metadata,
  any state, owner-or-elevated) and `DELETE /api/v1/blobs/{cid}` (soft-delete). New
  `GetBlobMeta`/`MarkSoftDeleted`/`ListOverdueSoftDeletes` sqlc queries in `blobs.sql`.
- **`GET /api/v1/admin/blobs`** (operator+moderator) — paginated operator-wide listing; new
  `ListBlobs`/`CountBlobs` queries (served by the existing `blobs_owner_state_idx` /
  `blobs_product_state_idx` / `blobs_uploaded_at_idx`); filters `state`/`product`/`owner_id`/
  `collection_id`; newest-first.
- **`GET /api/v1/admin/jobs`** (operator+moderator, **read-only**) — paginated queue introspection
  via a new inline-pgx `List`/`Count` on `internal/jobs` (served by `jobs_state_kind_idx`);
  filters `state`/`kind`; exposes `kind`/`state`/`attempts`/`last_error`/timestamps. **No retry**
  (see Out of scope).
- **Coordinator static-serving seam** — an optional handler that serves `web/admin/dist` at
  `/admin/*` (hashed assets `immutable`; `index.html` SPA-fallback for client routes; strict CSP
  `default-src 'self'`) when `NOVA_ADMIN_DIST_DIR` is set; `/admin/*` → `404` when unset
  (feature-gated like every other optional handler in `ServerConfig`).
- **The Admin SPA (`web/admin/`)** — React + Vite + TypeScript; React Router; TanStack Query;
  CSS Modules over the brand design tokens; Radix primitives for accessible dialogs/menus;
  self-hosted IBM Plex (latin, OFL) with system fallbacks. Two auth drivers behind one interface:
  a **local-issuer** password→token driver (silent refresh) and a full **external-OIDC
  authorization-code + PKCE** driver, selected by `GET /api/v1/auth/config`. Screens: login,
  blobs (list/detail/soft-delete), moderation + DMCA + blocklist, integrity-audit failures, key
  rotation (master + signing, with confirm modals), jobs (read-only).
- **Frontend CI lane + Makefile targets** — `admin-build`/`admin-lint`/`admin-test`/`hermetic-spa`;
  a Node job in `ci.yml`; a hermetic-bundle audit that greps the built bundle for external origins
  and fails on any hit.
- Unit tests (Go + Vitest) + an nginx-fronted integration test
  (`internal/integration/m11_admin_spa_test.go`) proving the exit criteria.

### Out of scope (with the milestone/owner that holds each)

- **Jobs retry / requeue** — **a later additive fast-follow.** In Phase 1 the only registered job
  kind is `derivative_prewarm`; moderation/takedowns run synchronously in-tx and the tombstone +
  integrity sweeps run in-process — none flow through `jobs.Queue`. A dead prewarm job is a
  missing thumbnail the `/i/` read path lazily re-renders, so per-job retry is a convenience, not a
  sovereignty control. The read-only list (with `last_error`) satisfies the deliverable; retry is
  purely additive (no rework) once a real operational need appears.
- **Owner blob metadata *update*** (`PATCH /api/v1/blobs/{cid}`) and **all `/api/v1/images/{cid}`
  CRUD** — **deferred.** The roadmap names "list/view/soft-delete", not edit; image-specific
  metadata stays with a later REST pass.
- **`/api/v1/collections/*`, `/api/v1/search/perceptual`** — **deferred** (collections UI is not an
  M11 surface; perceptual search is operator tooling, Phase 3 for the visual blocklist).
- **`clear-legal-hold` UI** — **Phase 4** (the M9 deferral); the endpoint exists, but the SPA does
  not surface legal-hold clearance. Severe-content holds are cleared via `novactl` in Phase 1.
- **Federation admin surfaces** (`/api/v1/admin/nodes`, `/admin/pins`, `/admin/audits/possession`)
  — **Phase 2.** They are documented in the openapi but unmounted; the SPA does not render them.
- **Production nginx templating + the two-vhost public/admin split + Docker packaging** — **M13.**
  M11 serves the SPA from the coordinator behind `/admin` and uses nginx's existing `location
  /admin` proxy (`nginx/nova.conf.example`) for its integration test. The hardened admin-origin
  separation is M13's deliverable.
- **The upload widget** (`web/widget/`, Uppy + tus) — **M12.** M11 ships no upload UI; uploads stay
  on `novactl`/the API for now.
- **Setup wizard** (`web/setup/`) — **M13.**
- **`operator.yaml` decode for the M11 knobs** — still deferred (M5–M10 precedent); env knobs only.

## Source of truth and required doc reconciliations

1. **`docs/specs/openapi.yaml` — mount two documented routes, add three paths.** Confirm
   `GET /api/v1/blobs/{cid}` (`getBlobMeta`) and `DELETE /api/v1/blobs/{cid}` (`deleteBlob`) match
   the new handlers (they are already specced; M11 makes them real). Add `GET /api/v1/admin/blobs`
   (`PaginatedBlobs`) and `GET /api/v1/admin/jobs` (`PaginatedJobs`), and document the coordinator
   static `/admin/*` surface as a non-API note. Keep the `oapi-codegen` + `sqlc` drift gates green.
2. **`docs/specs/DATA_MODEL.sql` — `blobs.soft_deleted_at` + the sweep index.** Add the column and
   the partial index, with a comment that owner soft-delete is the writer and the lifecycle sweep
   the reader; note that the soft-delete tombstone reuses the same crypto-shred path as moderation
   (one neutral primitive, two callers). Schema objects stay migration-authoritative; DATA_MODEL is
   the synchronized reference (M6 reconciliation precedent).
3. **`docs/THREAT_MODEL.md` — the hermetic-asset boundary + the admin origin.** Record (or
   cross-link, if already present as a Tier-1 commitment) that **the admin SPA bundle makes no
   third-party requests at runtime — no CDN fonts, scripts, or analytics — and CI enforces it**, and
   that M11 serves the admin surface behind `/admin` on the single Phase-1 origin with the hardened
   two-vhost split deferred to M13.
4. **`docs/legal/OPERATOR_CHECKLIST.md` — the admin-SPA runbook.** Document `NOVA_ADMIN_DIST_DIR`
   (build + point the coordinator at `web/admin/dist`; unset ⇒ `/admin` disabled), the soft-delete
   grace knob and its "tombstone + crypto-shred is irreversible after the grace window" warning, and
   the login modes (local issuer vs external OIDC, the latter driven by `/auth/config`).
5. **`docs/ROADMAP.md` + the master plan — the M11 row.** Mark status, link this design + its
   implementation plan, record the `m11-admin-spa` tag on completion, and record the deferrals
   (jobs retry → fast-follow, blob PATCH / images CRUD / collections / perceptual search → later,
   clear-legal-hold UI → Phase 4, widget → M12, setup wizard + nginx templating → M13).
6. **`docs/specs/PRODUCT_MODULE_INTERFACE.md` (note only).** The owner soft-delete cascades through
   the same `product.OnDelete` seam moderation uses; no interface change, a one-line note that owner
   deletion is now a second caller of the cascade.

## Preconditions from M1–M10 (confirmed in committed code)

- **Router + nil-gated handler pattern** (`internal/api/server.go`): `ServerConfig` carries optional
  `*handlers.*Admin` pointers; a nil pointer leaves the route falling through to `adminNotFound`
  (`:54–56`, `:178`). The `/admin` group runs `bearer.RequireRole("operator","moderator")` with
  inner `r.With(RequireRole("operator"))` tighteners for operator-only actions (`:139–153`). The
  identity-aware group (`bearer.Optional(cfg.Verifiers)`, `:125`) is where `/users/me` lives behind
  `RequireAuthenticated` (`:129`) — the mount site for the new owner `/api/v1/blobs/{cid}` routes.
  Auth config is always served; local issuer endpoints 404 in external mode (`:101–122`).
- **Moderation tombstone machinery** (`internal/moderation/service.go`): `Tombstone` (`:159`) =
  `ListDerivativeCIDs` (pre-tx) → in one `pgx.Tx`: `InsertModerationDecision`,
  `ClearScheduledTombstone`, `SetBlobState('tombstoned')`, `cascade`, `ShredDEKsForBlobTree`
  (`zeros72`, legal-hold CHECK → `ErrLegalHold`, `:198–203`), `InsertRevocation('cid')`,
  `actionCaseRef`, `strikeOwner`, audit `dmca.tombstoned` → commit → best-effort `unpin` of parent +
  derivatives (`:230–233`). The **neutral subset** (`SetBlobState`, `cascade`, `ShredDEKsForBlobTree`,
  `InsertRevocation`, `unpin`) is what M11 extracts; the moderation-specific subset stays.
  `CascadeHook`/`Backend` types (`:24`,`:28`) are the dependency-inverted seams to relocate.
- **The shared queries already exist** (sqlc, `internal/db/queries/moderation.sql`): `SetBlobState`,
  `ListDerivativeCIDs`, `ShredDEKsForBlobTree`, `GetBlobForModeration` (state + owner projection),
  `ListOverdueTombstones` (the partial-index, legal-hold-filtering claim the soft-delete sweep
  mirrors). `InsertRevocation` lives in `signedurl.sql`. The sweep loop pattern is
  `internal/moderation/sweeper.go` (`Tick` → claim overdue → `Tombstone`).
- **Blobs schema** (`0001_init.sql:286`): `blobs (cid PK, encryption_key_id, parent_cid,
  derivative_*, owner_id, mime_type, byte_size, uploaded_at, state blob_state DEFAULT 'active',
  source_ip, product blob_product DEFAULT 'raw')` — **no `soft_deleted_at` yet** (M11 adds it).
  Indexes `blobs_owner_state_idx (owner_id, state) WHERE state<>'tombstoned'`,
  `blobs_uploaded_at_idx`, `blobs_product_state_idx (product, state)` already serve the admin list.
- **Storage reads** (`pkg/coordinator/storage/blob.go`): `Resolve` is the *public* path and rejects
  non-active states; the owner/admin metadata read needs a state-agnostic projection (the new
  `GetBlobMeta`), not `Resolve`. `BlobView` fields (cid, mime, size, product, uploaded_at,
  visibility, encrypted, owner) are the basis for the metadata response.
- **Jobs** (`internal/jobs/queue.go`, inline pgx; `0002_jobs.sql`): `jobs (id, kind, payload, state
  job_state, attempts, max_attempts, lease_until, not_before, last_error, created_at, updated_at)`
  partitioned monthly, with `jobs_state_kind_idx (state, kind, created_at DESC)` already present for
  admin introspection. The queue uses `q.pool.QueryRow`/`Exec` with raw SQL — the admin list follows
  the same inline-pgx style (a read-only `List`/`Count`), not sqlc.
- **Auth** (`internal/auth/{bearer,localissuer,oidc}`, `server.go`): the coordinator is a resource
  server; `bearer.Optional`/`RequireRole` verify any configured verifier (local issuer **and/or**
  external OIDC). `GET /api/v1/auth/config` always returns `{mode, issuer_url?, client_id?, scopes?}`
  so a client can pick local vs external and drive PKCE itself; `/auth/{login,refresh,logout,
  jwks.json}` return `404 external_oidc_active` in external mode. `/api/v1/users/me` returns the
  caller's role for the SPA's role-gating.
- **Coordinator wiring** (`pkg/coordinator/coordinator.go`): subsystems are built gated on
  `pool`/`backend`/`ks`; in-process workers (M8 Maintainer/Scheduler, M9 Sweeper, M10
  ResumeIfRotating) are started in `Run`; `/readyz` checks are registered as `ReadyCheck{Name,Fn}`;
  the `auditlog.Writer` is shared. The M11 lifecycle `Sweeper` joins the in-process workers; the
  static handler + three admin handlers join the `ServerConfig`.
- **CLI / audit / test harness** — `internal/auditlog` (`WriteTx`, dotted action names), `internal/
  dbtest` (Postgres testcontainer), `internal/blobfixture` (seed blobs with known plaintext/
  visibility), and the nginx-fronted integration pattern (`m7…`–`m10…_test.go`,
  `startCoordinatorWithNginxCfg`, `m6Login`, `doJSONAuth`, `seedAuthUser`).

## Architecture

```
coordinator.New (pool + backend + ks present)
   ├─ lifecycle.NewService(gen.New(pool), pool, backend, cascade, auditW, slog, now, grace)
   ├─ lifecycle.NewSweeper(svc, interval, enabled, slog)         // sibling to moderation.Sweeper
   ├─ handlers.NewBlobMeta(storage, lifecycle)                   // GET + DELETE /api/v1/blobs/{cid}
   ├─ handlers.NewBlobsAdmin(gen.New(pool))                      // GET /api/v1/admin/blobs
   ├─ handlers.NewJobsAdmin(jobs.NewAdminStore(pool))            // GET /api/v1/admin/jobs (read-only)
   ├─ handlers.NewAdminSPA(NOVA_ADMIN_DIST_DIR)                  // /admin/* static (nil if unset)
   └─ moderation.NewService(... lifecycle.TombstoneTree ...)     // refactored to share the primitive

coordinator.Run
   └─ go lifecycleSweeper.Run(ctx)     // beside modSweeper / audit Maintainer+Scheduler / ResumeIfRotating

Owner soft-delete (DELETE /api/v1/blobs/{cid}, owner-or-operator):
   lifecycle.SoftDelete(cid, actor): one tx → MarkSoftDeleted(active→soft_deleted, soft_deleted_at=now)
                                     → cascade(soft_deleted) → audit blob.soft_deleted → 204

Lifecycle sweep (in-process, ~1 min):
   ListOverdueSoftDeletes(now-grace)  // state=soft_deleted, soft_deleted_at<cutoff, legal-hold tree filtered
   per cid:  TombstoneTree(tx, cascade, cid)  // SetBlobState(tombstoned)+cascade+ShredDEKsForBlobTree+InsertRevocation
             audit blob.tombstoned; commit; UnpinTree(parent + derivatives)

GET /api/v1/admin/blobs (operator+moderator):  ListBlobs(filters,page) + CountBlobs → PaginatedBlobs
GET /api/v1/admin/jobs   (operator+moderator):  jobs.AdminStore.List(filters,page) + Count → PaginatedJobs
GET /admin/*  (no API auth; behind /admin):     serve web/admin/dist (index.html fallback, strict CSP)
```

The SPA is a pure client of the `/api/v1` surface; it holds no privilege the API doesn't already
enforce. The lifecycle package **owns the write side of content destruction** for the non-moderation
path and shares the irreversible primitive with moderation, so crypto-shred lives in exactly one
place. Static serving is deliberately a thin, optional coordinator concern in M11 (the converging
path to M13's nginx-direct serving), gated exactly like the other optional handlers.

### Package boundaries

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/lifecycle` | `TombstoneTree` (neutral primitive), `SoftDelete`, `Sweeper`, `UnpinTree`, `CascadeHook`/`Backend` types, `ErrLegalHold` | `db/gen`, `pgxpool`, `auditlog`, `ipfs` (Unpin), `go-cid`, `slog` |
| `internal/moderation` | refactor `Tombstone` to call `lifecycle.TombstoneTree`; keep decision/case/strike/audit | `lifecycle`, `db/gen`, `auditlog` |
| `internal/db` (`blobs.sql` → `gen`) | `GetBlobMeta`, `MarkSoftDeleted`, `ListOverdueSoftDeletes`, `ListBlobs`/`CountBlobs` | — |
| `internal/jobs` (`admin.go`) | read-only `AdminStore.List`/`Count` (inline pgx, `jobs_state_kind_idx`) | `pgxpool` |
| `internal/api/handlers` | `BlobMeta` (GET+DELETE), `BlobsAdmin`, `JobsAdmin`, `AdminSPA` (static) | `lifecycle`, `storage`, `jobs`, `bearer`, `httputil` |
| `internal/api` (`server.go`) | mount owner blob routes + two admin lists + `/admin/*` static; new `ServerConfig` fields | `handlers` |
| `pkg/coordinator` | build lifecycle Service + Sweeper + handlers; start the sweep; wire `NOVA_ADMIN_DIST_DIR` | `lifecycle`, `jobs`, `moderation` |
| `cmd/coordinator` | `NOVA_ADMIN_DIST_DIR`, `NOVA_SOFT_DELETE_GRACE_SECONDS`, sweep-interval env knobs | `coordinator` |
| `web/admin` | the SPA (React+Vite+TS); openapi-typescript client; two auth drivers; screens | — (hermetic; bundled deps only) |

`internal/moderation` imports `internal/lifecycle` (the primitive authority); `lifecycle` does not
import `moderation` — no cycle. `lifecycle` defines `CascadeHook`/`Backend`; `moderation` aliases
them (`type CascadeHook = lifecycle.CascadeHook`) so its `NewService` signature is untouched and the
coordinator wires one `product.OnDelete` cascade to both.

## The content-lifecycle primitive (the key refactor)

Owner soft-delete must reach the *same* irreversible end state as a moderation takedown —
`tombstoned`, DEK crypto-shredded, derivatives cascaded, revocation written, Kubo unpinned — but it
is **not** a moderation action: it carries no rule, no DMCA case, no repeat-infringer strike, and a
distinct audit vocabulary. Duplicating the crypto-shred to keep the semantics separate would be the
wrong trade (two places that zero key material). Instead M11 extracts the neutral steps once:

```go
// internal/lifecycle/tombstone.go
type CascadeHook func(ctx context.Context, tx pgx.Tx, parentCID, newState string) error
type Backend interface{ Unpin(ctx context.Context, c cid.Cid) error }

// TombstoneTree performs the irreversible, caller-agnostic destruction inside an existing tx:
//   SetBlobState(tombstoned) → cascade(tombstoned) → ShredDEKsForBlobTree(zeros72) → InsertRevocation(cid)
// A legal-hold DEK raises no_shred_under_legal_hold (SQLSTATE 23514) → ErrLegalHold (caller rolls back).
func TombstoneTree(ctx context.Context, q *gen.Queries, tx pgx.Tx, cascade CascadeHook, cidStr string) error
```

`moderation.Tombstone` keeps its pre/post work and calls `TombstoneTree` for the middle; its
existing tests pass unchanged (behaviour identical, the steps relocated). The owner path adds no new
crypto — it reuses `ShredDEKsForBlobTree` and `zeros72` exactly.

### Owner soft-delete + the sweep

```
DELETE /api/v1/blobs/{cid}   (owner of the blob, OR operator; moderator may read but not delete)
  lifecycle.SoftDelete(ctx, cid, actor):
    one tx: MarkSoftDeleted(cid)         -- UPDATE blobs SET state='soft_deleted', soft_deleted_at=now()
                                         --   WHERE cid=$1 AND state='active'   (0 rows ⇒ ErrNotActive/404)
            cascade(cid, 'soft_deleted') -- derivatives follow the parent out of service
            audit blob.soft_deleted
    → 204
```

Soft-delete is **reversible during the grace window** (the bytes and DEK still exist; a future owner
"restore" or operator action could revert it — M11 ships the forward path; restore-from-soft-delete
is a trivial later add if desired). After the grace window the sweep makes it permanent:

```
lifecycle.Sweeper.Tick (every ~1 min):
  ListOverdueSoftDeletes(cutoff = now - grace)   -- state='soft_deleted' AND soft_deleted_at < cutoff,
                                                 --   filtering trees whose DEK carries legal_hold
                                                 --   (mirrors ListOverdueTombstones; the CHECK is the backstop)
  per cid in batch:
    one tx: TombstoneTree(tx, cascade, cid); audit blob.tombstoned
    commit; UnpinTree(cid + derivatives)         -- best-effort, after commit
```

`soft_deleted_at` is the new column the sweep ages against; without it the cutoff is uncomputable
(hence migration `0009`). The grace window is a config knob (`NOVA_SOFT_DELETE_GRACE_SECONDS`),
distinct from moderation's 14-day DMCA default — owner deletion is an intentional act, so a short
default (e.g. 24 h) is appropriate, leaving a recovery window without indefinitely retaining
"deleted" bytes. The sweep is dep-gated and disabled-safe exactly like `moderation.Sweeper`.

### Rows the sweep does not destroy

- **`legal_hold` DEK trees** — filtered out of the claim (the `no_shred_under_legal_hold` CHECK is
  the hard backstop). An owner cannot delete content a moderator has placed under legal hold; it
  stays `soft_deleted` until the hold clears, then ages out normally.
- **`public_archival` blobs** (`encryption_key_id IS NULL`) — `ShredDEKsForBlobTree` no-ops (no DEK);
  the blob still tombstones and unpins. The bytes are plaintext-addressed and were never secret.
- **Already-`tombstoned`/`shredded`** rows — not in `state='soft_deleted'`; never re-claimed.

## Admin list endpoints

### `GET /api/v1/admin/blobs` (operator + moderator)

A paginated, operator-wide projection for the blob-management screen. New sqlc `ListBlobs` /
`CountBlobs` with optional `state` (`active|soft_deleted|quarantined|tombstoned`), `product`,
`owner_id`, `collection_id` filters, ordered `uploaded_at DESC`, served by `blobs_owner_state_idx` /
`blobs_product_state_idx` / `blobs_uploaded_at_idx`. Each row carries `cid, owner_id, product, state,
mime_type, byte_size, uploaded_at, parent_cid` (enough for the table and a "derivative of" badge).
The SPA navigates a row → `GET /api/v1/blobs/{cid}` for the detail view. Response shape
`PaginatedBlobs` mirrors the existing `PaginatedModerationDecisions` envelope (`items`, `page`,
`per_page`, `total`).

### `GET /api/v1/admin/jobs` (operator + moderator, read-only)

Queue introspection for the jobs screen — `state` (`pending|leased|completed|failed|dead`) and
`kind` filters, newest-first, served by `jobs_state_kind_idx`. A read-only inline-pgx
`jobs.AdminStore.List(ctx, filter, limit, offset)` / `Count` (the queue is inline-pgx, so the admin
read matches; no sqlc for jobs). Each row carries `id, kind, state, attempts, max_attempts,
last_error, not_before, lease_until, created_at, updated_at`. **No mutation endpoint** — the screen
shows stuck/failed/recent work and `last_error`; requeue is a deliberate non-goal for M11.

## Coordinator static serving (the SPA seam)

`ServerConfig` gains `AdminDistDir string`. When non-empty the router mounts a `/admin/*` static
handler; when empty `/admin/*` 404s (the SPA simply isn't served — the milestone-gate posture). The
handler:

- serves files from `<dist>` with `Cache-Control: public, max-age=31536000, immutable` for Vite's
  content-hashed assets (`/admin/assets/*`), and `Cache-Control: no-store` for `index.html`;
- **SPA fallback:** any `/admin/*` path that is not an existing asset returns `index.html` (so deep
  links like `/admin/blobs/<cid>` work under client-side routing) — but never serves `index.html`
  for an `/api/...`-shaped path (distinct prefixes; no overlap with `/api/v1/admin`);
- emits a **strict CSP** matching `nginx/nova.conf.example`: `default-src 'self'; img-src 'self'
  data:; style-src 'self' 'unsafe-inline'; font-src 'self'; connect-src 'self'; frame-ancestors
  'none'; base-uri 'none'`, plus `X-Content-Type-Options: nosniff`. (`'unsafe-inline'` for styles
  only, scoped to CSS Modules' injected styles; scripts are `'self'` only — the hermetic bundle has
  no inline scripts.)

This is the M11-testable serving path. M13 will template nginx to serve the bundle directly on a
separate admin vhost; the coordinator handler remains the dev/self-contained path. `/admin` (static)
and `/api/v1/admin` (API) never collide.

## The Admin SPA (`web/admin/`)

**Stack.** React + Vite + TypeScript; React Router (client routing under `/admin`); TanStack Query
(fetch/cache/invalidate); CSS Modules layered over a `tokens.css` generated from the brand design
tokens (`docs/design/Nova Brand _standalone_.html`: IBM Plex; `--paper #ECE8DF`, `--card #F4F1E8`,
ink tones, `--nova #E2502B` accent, `--ok #4A6B3A`, slate/rule neutrals); Radix UI primitives only
where accessibility demands a managed widget (Dialog for rotation/soft-delete confirms, DropdownMenu,
Toast). The API client is generated from `docs/specs/openapi.yaml` via `openapi-typescript` (the same
toolchain M12's widget will use), so the SPA's types track the spec and CI's drift gate covers them.

**Hermetic by construction.** No third-party runtime requests: IBM Plex Sans/Serif/Mono are
**self-hosted** (latin subset, OFL) via `@fontsource` and bundled with hashed filenames; the brand
HTML's Google-Fonts `preconnect` is a design-reference artifact and is *not* reproduced. `vite.config.ts`
sets a relative `base: '/admin/'`, inlines nothing external, and the `hermetic-spa` gate greps
`web/admin/dist` for `http://`/`https://`/`//` origins and fails the build on any hit. If self-hosting
IBM Plex ever complicates CSP/licensing/reproducibility, the fallback is the cross-OS system stack the
brand tokens already encode (`ui-sans-serif, system-ui, …` / `ui-serif, Georgia, …` /
`ui-monospace, …`) — **local-only assets are the invariant; IBM-Plex-vs-system is negotiable.**

**Auth — two drivers behind one interface.** On boot the SPA calls `GET /api/v1/auth/config` and
selects:

- `LocalAuthDriver` — `POST /auth/login {username,password}` → access (15 m) + refresh (12 h) in
  `localStorage`; a timer refreshes via `POST /auth/refresh` (rotation; reuse revokes the family)
  shortly before access expiry; `POST /auth/logout` on sign-out. This is the Phase-1 default and the
  common case; it is *not* real PKCE (the roadmap's "PKCE-style" is loose wording — the design doc of
  record is M6).
- `OidcPkceDriver` — for `mode=external`: a standard browser **authorization-code + PKCE** flow
  against the operator's IdP (`issuer_url`/`client_id`/`scopes` from `/auth/config`): generate
  `code_verifier`/`code_challenge`, redirect to the IdP authorize endpoint, handle the `/admin`
  callback, exchange the code for tokens at the IdP token endpoint, store + silently renew. PKCE
  runs **client↔IdP**; Nova only ever receives the resulting bearer token through its existing
  verifiers — **zero new coordinator surface.** The local issuer's `404 external_oidc_active` and the
  always-on `/auth/config` are the infrastructure M6 built for exactly this.

A single `AuthProvider` exposes `{user, login, logout, authedFetch}`; screens never branch on mode.

**Shell + screens.** An app shell with sidebar nav (mirroring the brand "Operator surfaces" mockup),
a header showing the current user (`GET /api/v1/users/me`) + logout, and a content outlet. The SPA is
**role-aware**: it reads the caller's role and hides/disables operator-only controls (rotate-master,
rotate-signing) for moderators — defense-in-depth only; the server enforces with `403` regardless.

| Route | Surface | API |
|---|---|---|
| `/admin/login` | local form or "Continue with <IdP>" per `/auth/config` | `/auth/*`, `/auth/config` |
| `/admin/blobs` | list + filters (state/product/owner) + pagination | `GET /api/v1/admin/blobs` |
| `/admin/blobs/:cid` | detail + preview + soft-delete (confirm modal) | `GET /api/v1/blobs/{cid}`, `DELETE /api/v1/blobs/{cid}` |
| `/admin/moderation` | queue, takedown/quarantine/restore/counter-notice, DMCA cases, CID blocklist | `/admin/moderation/*`, `/admin/dmca*` |
| `/admin/audits` | integrity-audit failures (`result=fail`) | `GET /api/v1/admin/audits/integrity` |
| `/admin/keys` | master rotate + status poll; signing rotate (operator-only; confirm modals) | `/admin/keys/*` |
| `/admin/jobs` | read-only queue table (state/kind, `last_error`) | `GET /api/v1/admin/jobs` |
| `/admin/audit-log` | *optional, scope-permitting* — privileged-action log viewer | `GET /api/v1/admin/audit-log` |

## HTTP contract

### Owner blob routes (`/api/v1`, identity-aware group)

| Method | Path | Auth | Result |
|---|---|---|---|
| GET | `/blobs/{cid}` | `RequireAuthenticated` (owner or operator/moderator) | `200 Blob` (any state) |
| DELETE | `/blobs/{cid}` | `RequireAuthenticated` (owner or operator) | `204` soft-delete accepted |

Authorization is in-handler: the caller must be the blob's `owner_id` or hold `operator`
(moderators may `GET` but not `DELETE`). `ServerConfig` gains a nil-able `BlobMeta
*handlers.BlobMetaHandler`.

### Admin lists (`/api/v1/admin/*`, operator + moderator)

| Method | Path | Result |
|---|---|---|
| GET | `/admin/blobs` | `200 PaginatedBlobs` (filters: `state`, `product`, `owner_id`, `collection_id`, `page`, `per_page`) |
| GET | `/admin/jobs` | `200 PaginatedJobs` (filters: `state`, `kind`, `page`, `per_page`) |

Nil-able `BlobsAdmin` / `JobsAdmin` pointers; nil ⇒ `adminNotFound`.

### Error → status

| Condition | Status | `code` |
|---|---|---|
| no bearer (API) | 401 | `unauthenticated` |
| authenticated, wrong role/owner | 403 | `forbidden` |
| `GET/DELETE /blobs/{cid}` unknown cid | 404 | `not_found` |
| `DELETE` a non-active blob (already soft-deleted/tombstoned) | 409 | `not_active` |
| bad pagination/filter | 400 | `invalid_request` |
| `/admin/*` static, `NOVA_ADMIN_DIST_DIR` unset | 404 | (static 404, not JSON) |

## Configuration

```go
// internal/config/types.go
type AdminSPA struct {
    DistDir string `yaml:"dist_dir"` // serve web/admin/dist at /admin/*; empty ⇒ /admin disabled
}
type ContentLifecycle struct {
    SoftDeleteGrace time.Duration `yaml:"soft_delete_grace"` // soft_deleted → tombstone+shred; default 24h
    SweepInterval   time.Duration `yaml:"sweep_interval"`    // sweep cadence; default 1m (mirrors moderation)
}
```

`cmd/coordinator/main.go` reads `NOVA_ADMIN_DIST_DIR` (unset ⇒ disabled),
`NOVA_SOFT_DELETE_GRACE_SECONDS` (default 86400), and `NOVA_LIFECYCLE_SWEEP_INTERVAL_MS` (default
60000), each falling back when unset/zero — the M7–M10 env precedent. `operator.yaml` decode stays
deferred.

## The audit vocabulary

Best-effort `auditlog.Writer.WriteTx` inside the lifecycle transaction (the M9 pattern), with a
vocabulary **distinct from moderation's `dmca.*`/`severe.*`** so owner deletion is never confused with
a takedown:

| Action | When | Actor | Payload |
|---|---|---|---|
| `blob.soft_deleted` | `DELETE /api/v1/blobs/{cid}` accepted (**handler tx**) | the caller | `{cid}` |
| `blob.tombstoned` | the lifecycle sweep crypto-shreds an overdue soft-delete (**Sweeper**, system) | `nil` (system) | `{cid, grace_seconds}` |

`TargetType = "cid"`, `TargetID = <cid>`. Payloads never carry key material. Actor attribution
follows the M9/M10 layering: the handler writes `blob.soft_deleted` with the request actor; the
background sweeper writes `blob.tombstoned` as a system action (`ActorID = nil`, exactly like the M9
sweep's `Tombstone(actor=nil)`).

## Security and privacy considerations

- **Hermetic admin bundle (enforced).** The SPA makes no third-party runtime requests — fonts are
  self-hosted, scripts are first-party, there is no telemetry — and the `hermetic-spa` CI gate fails
  the build on any external origin in `dist`. The coordinator's strict CSP (`default-src 'self'`) is
  the runtime backstop. This is the Tier-1 "no third-party CDN assets at runtime" commitment made
  executable.
- **PKCE adds no coordinator surface.** External-OIDC PKCE is a browser↔IdP exchange; Nova remains a
  resource server verifying bearer tokens it already accepts. The local-issuer path is isolated
  behind its own driver so the common case is never coupled to external-mode edge cases.
- **Tokens in `localStorage` (honest caveat).** The local-issuer driver stores access+refresh in
  `localStorage` with silent rotation, per the M6 design. This is the documented Phase-1 posture; the
  admin origin's strict CSP (`script-src 'self'`, `frame-ancestors 'none'`) and the hermetic bundle
  are the XSS-exfiltration mitigations. Short access TTL (15 m) + refresh rotation bound the blast
  radius. (HttpOnly-cookie sessions are a possible later hardening, noted not built.)
- **Soft-delete is irreversible after grace (by design, audited).** The sweep crypto-shreds via the
  same primitive as a takedown; the grace window is the only recovery affordance, and both the
  request and the shred are audit-logged. The runbook documents that dropping the grace to near-zero
  forfeits recovery.
- **Owner cannot escape legal hold.** A soft-deleted blob under legal hold is filtered out of the
  sweep and protected by the `no_shred_under_legal_hold` CHECK — owner deletion can never shred
  held key material.
- **Role enforcement is server-side.** The SPA hiding operator-only controls is cosmetic;
  `RequireRole("operator")` on the endpoints is the boundary (a moderator calling rotate-master gets
  `403` regardless of the UI).

## Exit criteria

1. With `NOVA_ADMIN_DIST_DIR` set, `GET /admin/` (through nginx) returns the SPA `index.html` with the
   strict CSP and **no external-origin references** in the bundle (`make hermetic-spa` passes); a deep
   link (`/admin/blobs`) also returns `index.html` (client routing). With it unset, `/admin/*` → 404.
2. An operator logs in via the SPA (local issuer), lists blobs (`GET /api/v1/admin/blobs`), opens one
   (`GET /api/v1/blobs/{cid}`), and soft-deletes it (`DELETE`). The blob immediately reads `soft_deleted`
   (public read 410); **after the grace window** the lifecycle sweep tombstones it — the read stays 410
   and its DEK is `state='shredded'`. The `audit_log` shows `blob.soft_deleted` then `blob.tombstoned`
   (lifecycle actions, **not** `dmca.*`).
3. `GET /api/v1/admin/jobs` returns queue rows read-only with `last_error`; there is no mutation/retry
   endpoint. `GET /api/v1/admin/blobs` paginates and filters by `state`/`product`/`owner`.
4. In external-OIDC mode, `/auth/config` drives the SPA's authorization-code + PKCE flow; a token
   minted via the external IdP is accepted by the coordinator; local `/auth/*` return `404
   external_oidc_active`.
5. Authorization holds end-to-end: a moderator gets `403` on operator-only actions (rotate-master,
   rotate-signing) and on `DELETE /blobs/{cid}`; an unauthenticated caller gets `401`; the SPA hides
   the operator-only controls for moderators.
6. `moderation.Tombstone` is unchanged in behaviour after the `lifecycle.TombstoneTree` extraction
   (the M9 suite passes), and crypto-shred exists in exactly one place.

## Testing strategy

### Unit (Go)

- **`internal/lifecycle`** (`dbtest` + `blobfixture`): `SoftDelete` flips `active → soft_deleted`
  with `soft_deleted_at` set and cascades to a derivative; `DELETE` on a non-active blob →
  `ErrNotActive`. The `Sweeper` over a seeded overdue soft-delete tombstones + shreds the DEK tree
  (assert `state='shredded'`, `wrapped_key == zeros72`) and unpins (recording fake backend); a
  legal-hold tree is **skipped** (stays `soft_deleted`, no shred); a `public_archival` (no-DEK) blob
  tombstones without error. `TombstoneTree` is exercised directly for the legal-hold `ErrLegalHold`
  rollback.
- **`internal/moderation`** — the existing suite must pass unchanged after the refactor (the
  behavioural guarantee).
- **`internal/jobs`** — `AdminStore.List`/`Count` over seeded jobs across states/kinds, pagination,
  newest-first.
- **Handlers** — `BlobMeta` (owner 200; non-owner 403; operator 200; `DELETE` owner/operator 204,
  moderator 403, non-active 409, unknown 404); `BlobsAdmin`/`JobsAdmin` (authz 401/403, filters,
  pagination shape); `AdminSPA` (asset vs index.html fallback, CSP header present, `NOVA_ADMIN_DIST_DIR`
  unset ⇒ 404).

### Frontend (Vitest)

- Auth drivers: local refresh-rotation timing and logout; the `OidcPkceDriver` `code_verifier`/
  `code_challenge` derivation and the callback token-exchange against a **mocked IdP**; driver
  selection from `/auth/config`.
- API client + screens: table render/filter/pagination; role-gated control visibility (operator vs
  moderator); the soft-delete confirm flow; error/empty states.

### Integration (`internal/integration/m11_admin_spa_test.go`, nginx-fronted, testcontainers)

Boot the coordinator with `AdminDistDir` pointed at a freshly built `web/admin/dist` + nginx (the
existing `location /admin` proxy), then drive the surface the SPA depends on **end-to-end through
nginx** (mirroring M10's `startCoordinatorWithNginxCfg` / `m6Login` / `doJSONAuth` /
`seedAuthUser` / `blobfixture.Seed`):

1. **Serving:** `GET /admin/` → 200 `index.html` with strict CSP; `GET /admin/blobs` (deep link) →
   `index.html`; the bundle references no external origin (assert against `dist`); coordinator booted
   with `AdminDistDir=""` → `/admin/` 404.
2. **Lifecycle (Exit #2):** local login → `GET /api/v1/admin/blobs` (seeded) → `GET /api/v1/blobs/{cid}`
   → `DELETE` → public read 410 (`soft_deleted`); advance the clock / short grace and run the sweep →
   read still 410, DEK `shredded`, `audit_log` has `blob.soft_deleted` then `blob.tombstoned`.
3. **Lists:** `GET /api/v1/admin/jobs` read-only with `last_error`; `GET /api/v1/admin/blobs` filters.
4. **External-OIDC (Exit #4):** boot with an external verifier; `/auth/config` advertises the IdP;
   a token from a stub IdP is accepted; local `/auth/login` → `404 external_oidc_active`.
5. **Authz (Exit #5):** moderator → 403 on rotate-master and `DELETE /blobs/{cid}`; no token → 401.

### CI

- New Node job: `make admin-build && make admin-lint && make admin-test && make hermetic-spa`.
- `make sqlc-generate && make codegen-check` after `blobs.sql`; `oapi-codegen` drift after the openapi
  additions; `0009` migration applies cleanly (`make smoke`).
- `-short`-skippable integration like M2–M10; gofmt only the files M11 touches (toolchain-skew rule);
  `golangci-lint`/`eslint` are CI-side.

## File structure

### Created in M11

```
internal/lifecycle/tombstone.go                     TombstoneTree (neutral primitive), CascadeHook/Backend types, ErrLegalHold/ErrNotActive
internal/lifecycle/service.go                       SoftDelete + UnpinTree
internal/lifecycle/sweeper.go                       overdue-soft-delete sweep (sibling to moderation.Sweeper)
internal/lifecycle/*_test.go
internal/jobs/admin.go                              read-only AdminStore.List/Count (inline pgx, jobs_state_kind_idx)
internal/jobs/admin_test.go
internal/db/migrations/0009_blob_soft_delete.sql    blobs.soft_deleted_at + partial sweep index
internal/api/handlers/blob_meta.go                  GET + DELETE /api/v1/blobs/{cid}
internal/api/handlers/blobs_admin.go                GET /api/v1/admin/blobs
internal/api/handlers/jobs_admin.go                 GET /api/v1/admin/jobs (read-only)
internal/api/handlers/admin_spa.go                  /admin/* static (dist serving, CSP, SPA fallback)
internal/api/handlers/*_test.go
internal/integration/m11_admin_spa_test.go          nginx-fronted end-to-end (the exit criteria)
web/admin/                                           the SPA: package.json, vite.config.ts, tsconfig, index.html, src/* (shell, auth drivers, screens, tokens.css), tests
docs/superpowers/specs/2026-06-04-phase1-m11-admin-spa-design.md   (this file)
docs/superpowers/plans/2026-06-04-phase1-m11-admin-spa.md          (the implementation plan)
```

### Modified in M11

```
internal/moderation/service.go     Tombstone refactored to call lifecycle.TombstoneTree; CascadeHook/Backend aliased to lifecycle
internal/db/queries/blobs.sql       GetBlobMeta, MarkSoftDeleted, ListOverdueSoftDeletes, ListBlobs, CountBlobs
internal/db/gen/*                   regenerated from blobs.sql
internal/api/server.go              mount /api/v1/blobs/{cid} GET+DELETE; /admin/blobs; /admin/jobs; /admin/* static; new ServerConfig fields
pkg/coordinator/coordinator.go      build lifecycle Service+Sweeper + BlobMeta/BlobsAdmin/JobsAdmin/AdminSPA handlers; start the sweep; AdminSPA + ContentLifecycle config
cmd/coordinator/main.go             NOVA_ADMIN_DIST_DIR, NOVA_SOFT_DELETE_GRACE_SECONDS, NOVA_LIFECYCLE_SWEEP_INTERVAL_MS
internal/config/types.go            AdminSPA + ContentLifecycle sections
Makefile                            admin-build, admin-lint, admin-test, hermetic-spa
.github/workflows/ci.yml            Node job (build/lint/test/hermetic-spa)
package.json                        web/admin workspace scripts (already a declared workspace)
docs/specs/openapi.yaml             mount blobs/{cid} GET+DELETE; add /admin/blobs + /admin/jobs (+ PaginatedBlobs/PaginatedJobs)
docs/specs/DATA_MODEL.sql           blobs.soft_deleted_at + sweep index (reconciliation #2)
docs/THREAT_MODEL.md                hermetic-asset boundary + /admin origin note (reconciliation #3)
docs/legal/OPERATOR_CHECKLIST.md    admin-SPA runbook: NOVA_ADMIN_DIST_DIR, soft-delete grace, login modes (reconciliation #4)
docs/ROADMAP.md                     M11 status + tag + deferrals (reconciliation #5)
docs/specs/PRODUCT_MODULE_INTERFACE.md  one-line: owner deletion is a second cascade caller (reconciliation #6)
```

### Reused unchanged

```
internal/db/queries/moderation.sql  SetBlobState, ListDerivativeCIDs, ShredDEKsForBlobTree, GetBlobForModeration
internal/db/queries/signedurl.sql   InsertRevocation (the ('cid', cid) revocation on tombstone)
internal/moderation/sweeper.go      the in-process sweep pattern the lifecycle Sweeper mirrors
internal/auditlog/writer.go         best-effort WriteTx (dotted action names)
internal/auth/{bearer,localissuer,oidc}   RequireRole/RequireAuthenticated, /auth/config, verifiers
internal/api/httputil               WriteError (Error shape)
pkg/coordinator/storage             Resolve/BlobView (public read path; the metadata read borrows its fields)
internal/dbtest, internal/blobfixture, internal/integration   test harness
docs/design/Nova Brand _standalone_.html   brand tokens (design reference for tokens.css)
```

## Risks and notes

- **Soft-delete is the long pole, not the SPA (sequence accordingly).** The lifecycle primitive +
  sweep + migration are the load-bearing backend work; land and prove them (DEK shredded after grace,
  moderation suite still green) before frontend polish. The React app is comparatively mechanical.
- **Don't let owner deletion bleed into moderation semantics (decided).** Share only
  `TombstoneTree`; keep audit actions (`blob.*` vs `dmca.*`/`severe.*`), authorization, grace, and the
  decision/strike bookkeeping distinct. The refactor must leave `moderation.Tombstone` behaviourally
  identical.
- **`localStorage` token storage (accepted, documented).** Per the M6 design; mitigated by the
  hermetic bundle + strict CSP + short access TTL + refresh rotation. A cookie-session hardening pass
  is noted, not built.
- **Build-ordering for serving (resolved).** Dir-served (`NOVA_ADMIN_DIST_DIR`) rather than
  `//go:embed` keeps `go build`/`go test` decoupled from a prior SPA build — important since the
  frontend CI lane is new this milestone. The integration test builds `dist` explicitly before
  booting.
- **External-OIDC PKCE edge cases (bounded).** Redirect/callback/renewal complexity is contained
  behind `OidcPkceDriver`; the local path is the default and is unaffected. A stub IdP covers the flow
  in tests; a real IdP (Authelia) is an operator human-action check.
- **Hermetic fonts (decided).** Self-host IBM Plex (latin/OFL); if licensing/CSP/reproducibility get
  hairy, fall back to the system stack the tokens already encode. Local-only is the invariant.
- **Jobs view shows little in Phase 1 (accepted).** Only `derivative_prewarm` flows through the queue
  today; the screen is genuinely useful for stuck prewarms and is ready for future job kinds. Retry is
  a clean later add.

## Cross-references

- `docs/ROADMAP.md` M11 row + `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md`
  § "Walking-skeleton milestone breakdown" (M11) — the committed surface and the `web/admin` layout.
- `docs/specs/openapi.yaml` — `/api/v1/blobs/{cid}` (GET+DELETE), the new `/admin/blobs` + `/admin/jobs`,
  `/api/v1/auth/config` (the mode-discovery document the SPA drives).
- M9 design (`2026-06-02-phase1-m9-moderation-design.md`) — the tombstone/crypto-shred/unpin machinery
  M11 extracts into `internal/lifecycle`; the in-process sweep + `auditlog` conventions.
- M6 design (`2026-05-30-phase1-m6-auth-design.md`) § "Mode selection" — the resource-server boundary
  and the SPA-drives-PKCE assignment M11 fulfils; the local-issuer token lifecycle.
- `internal/api/server.go` — the nil-gated handler + `RequireRole` patterns the new routes follow.
- `docs/THREAT_MODEL.md` — the hermetic-asset Tier-1 commitment + the admin-origin boundary.
- `docs/superpowers/plans/2026-06-04-phase1-m11-admin-spa.md` — the implementation plan.
```
