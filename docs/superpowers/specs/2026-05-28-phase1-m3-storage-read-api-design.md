# Phase 1 M3 — Storage Core API (Read Path) Design

**Goal:** A coordinator process serves the anonymous read path end-to-end:
`GET /blob/{cid}` returns the **decrypted** bytes, `HEAD /blob/{cid}` returns
metadata headers without decrypting, `GET /blob/{cid}.json` returns public JSON
metadata, and `GET /health` returns 200 — all reachable through an nginx proxy.
The milestone composes the M2 envelope/IPFS/keystore subsystems behind a
read-only storage service under a stable `pkg/coordinator` lifecycle.

**Architecture (one line):** `cmd/coordinator` wires injected dependencies into
`pkg/coordinator`, which owns an HTTP server whose `internal/api` handlers call a
HTTP-naïve `pkg/coordinator/storage` service that resolves a blob (Postgres),
fetches its envelope (embedded Kubo), unwraps the per-blob key (keystore), and
decrypts (envelope v1) — returning typed errors the handler maps to status codes.

**Status:** approved design — implementation plan follows in
`docs/superpowers/plans/2026-05-28-phase1-m3-storage-read-api.md`.

**Author:** Bug Plowman (operator), Claude (implementation partner).

---

## Purpose and scope

M3 is the third Phase 1 milestone. M1 delivered the schema + config foundation;
M2 delivered the envelope codec, embedded hardened Kubo, the keystore, and the
job queue. M3 makes those reachable over HTTP for **reads only**. No write path,
no auth issuer, no image routes.

### In scope

- `pkg/coordinator` public library: `Config`, `New`, `Run`, `Shutdown`.
- `pkg/coordinator/storage` read service: `Service.Get`, typed errors, result
  types. HTTP-naïve and product-agnostic.
- `internal/api`: chi router + middleware (request-id, recover, rate-limit) and
  handlers for `/health`, `/blob/{cid}` (GET, HEAD), `/blob/{cid}.json`.
- `cmd/coordinator/main.go`: manual dependency wiring + startup validation +
  graceful shutdown.
- sqlc adoption for the read-path query surface (committed generated code + CI
  drift gate).
- `nova_dev` build-tag floor: production refuses `auth.anonymous: true`; the dev
  build permits it.
- A minimal static **dev** nginx proxy (`docker/nginx/`) that fronts the
  coordinator; the integration test fetches through it.
- Unit tests (storage, handlers, middleware), an nginx-fronted integration test,
  and a documented encrypted-read memory/size budget.

### Out of scope (with the milestone that owns each)

- Write path: tus + multipart upload, AnalyzeUpload, DB-commit transaction — **M4**.
- `oapi-codegen` generated handler types + CI diff gate — **M4** (M3's HTTP body
  surface is bytes + one small JSON object; hand-writing is cheaper than wiring
  codegen for three endpoints).
- Bearer auth issuance + middleware — **M6**.
- Signed-URL minting/verification + revocation — **M7**.
- `audit_log` middleware — **M6** (it records *privileged* actions; M3 has only
  anonymous public reads). See "Doc reconciliations".
- Image routes `/i/*`, `nova-image` product, `storage.PutDerivative` — **M5**.
- `RegisterProduct` API + registration-time prefix-collision enforcement — **M5**
  (no product exists until then; see "Coordinator lifecycle").
- Admin endpoints, `/api/v1/*`, `/legal/*`, `/fed/v1/*` — **M6+**.
- nginx TLS, dual public/admin origins, `proxy_cache`, per-IP rate limiting,
  config templating — **M7/M11/M13**.
- `docker/Dockerfile` + a coordinator service in `docker-compose.yml` — **M13**.
- Readiness endpoint, metrics counters — **M8/M14**. `/health` is liveness only.

---

## Source of truth and required doc reconciliations

This spec is authoritative for M3 behavior. Where existing Nova documents
conflict, this spec resolves the conflict and the losing document is corrected.
Two contradictions surfaced during design; both are resolved here and the edits
are applied after this spec is reviewed:

1. **Unlisted read policy.** `openapi.yaml:112` groups `private/unlisted` under
   "signed URL"; the design read-pipeline (`…single-node-mvp-design.md:457`) says
   `unlisted → no auth required (slug knowledge sufficient)`. **Resolution: the
   design wins — unlisted blobs are anonymously readable by CID.** A CID is a
   256-bit content hash (`Cid` pattern `^bafy[a-z0-9]{55,}$`), not enumerable, so
   knowledge of the CID is itself the capability, consistent with the "slug
   knowledge sufficient" stance. Edit: split row `openapi.yaml:112` so `unlisted`
   is anonymous and only `private` requires a signed URL.

2. **Streaming-AEAD phase placement.** `openapi.yaml:117-119` calls streaming
   AEAD a "Phase 6+ deliverable"; `ROADMAP.md:107-120,155` promoted it to
   **Phase 2**. Edit: correct the openapi comment to "Phase 2". No M3 behavior
   change (encrypted Range → 416 regardless).

3. **`audit_log` milestone.** `…single-node-mvp.md:57` lists `audit-log`
   middleware under M3. Edit: amend that line to note audit logging lands with
   the first privileged endpoints (**M6**); M3 has no privileged action to record.

---

## Preconditions from M2

M3 builds directly on these M2 capabilities. If any diverges from its committed
state, M3 estimates are optimistic and the seam must be reconciled first.

| Dependency | Symbol / location | M3 use |
|---|---|---|
| Envelope decode + decrypt | `envelope.Decode`, `envelope.Codec.Decrypt` (`internal/envelope`) | Decrypt fetched envelope bytes |
| Keystore unwrap | `(*envelope.Keystore).Unwrap(wrapped, versionID)` | Recover the per-blob key |
| Embedded IPFS backend | `ipfs.Backend.Get(ctx, cid)` via `EmbeddedBackend` (`internal/ipfs`) | Fetch envelope bytes by CID |
| Kubo hardening validator | `ipfs.ValidateConfig` | Run at startup before serving |
| DB pool | `db.Open(ctx, dsn)` → `*pgxpool.Pool` (`internal/db`) | Backing store for queries |
| Migrations | `internal/db/migrations` 0001–0004 incl. `blobs.envelope_version` | Schema the queries target |
| Config + secrets | `config.Load`, secrets resolver (`internal/config`) | operator.yaml + `NOVA_MASTER_KEY_*`, `DATABASE_URL` |

The M3 integration test inserts blob/manifest/key/collection rows **by hand**
(plus a real `AddDeterministic` import) to stand in for the M4 write path.

---

## Architecture

```
cmd/coordinator/main.go (+ wire.go)   ── manual DI: load config, resolve secrets,
        │                                 open pool, Keystore.Bootstrap, build +
        │ injects deps                    ValidateConfig + run EmbeddedBackend,
        ▼                                 enforce startup floor, trap SIGTERM
pkg/coordinator/coordinator.go        ── PUBLIC, semver-stable. Config, New(...),
        │ owns http.Server + storage      Run(ctx) (blocks), Shutdown(ctx) (graceful)
        ▼
pkg/coordinator/storage/              ── READ CORE. DB + Kubo + crypto. Returns
   blob.go    Service.Get                typed errors, NOT HTTP status. Product-agnostic.
   types.go   BlobResult, Visibility, CacheClass
   errors.go  ErrBlobNotFound / Quarantined / SoftDeleted / Tombstoned /
              KeyShredded / AuthRequired
        ▲ called by
internal/api/                         ── HTTP layer (hand-written)
   server.go        chi router + middleware stack; mounts handlers; reserves namespaces
   handlers/health.go   GET /health → Health JSON
   handlers/blob.go     GET+HEAD /blob/{cid}, GET /blob/{cid}.json
   errors.go        writeError(w, status, code, msg) → Error JSON with request_id
   middleware/      requestid.go, recover.go, ratelimit.go
internal/ratelimit/bucket.go          ── token bucket (defense-in-depth; nginx primary later)
internal/auth/anonymous_{dev,prod}.go ── nova_dev build-tag startup floor
internal/db/queries/*.sql + sqlc.yaml + gen/  ── sqlc read queries (gen committed)
docker/nginx/nova.dev.conf            ── minimal HTTP proxy → coordinator:9000
```

### Layering rationale

- **`storage.Service` is HTTP-naïve**, mirroring how `internal/envelope` is
  DB-naïve. It returns domain errors; only `internal/api` knows status codes.
  This keeps the read core reusable by the M5 image product and testable without
  an HTTP server.
- **`pkg/coordinator` takes injected dependencies.** Per
  `PRODUCT_MODULE_INTERFACE.md:190-212`, the documented constructor is
  `coordinator.New(coordinator.Config{ DB, … }) + coord.RegisterProduct(…)` —
  the pool is passed in, not opened internally. M3 follows that: `New` receives a
  `*pgxpool.Pool`, an `ipfs.Backend`, a `*envelope.Keystore`, and settings.
  `cmd/coordinator` performs all env/secrets/construction ("manual DI, no DI
  lib"). This matches Bug's DI preference and the Phase-0 contract.

### Coordinator lifecycle

```go
package coordinator

type Config struct {
    ListenAddr string          // ":9000"
    RateLimit  RateLimitConfig // per-IP token-bucket knobs (defense-in-depth)
    Version    string          // build version string for /health
    // M5 adds product registration; M6+ adds auth config.
}

func New(pool *pgxpool.Pool, backend ipfs.Backend, ks *envelope.Keystore, cfg Config) (*Coordinator, error)
func (c *Coordinator) Run(ctx context.Context) error      // builds router, blocks on http.Server until ctx done
func (c *Coordinator) Shutdown(ctx context.Context) error // graceful server shutdown; does NOT close injected deps
```

`New` constructs the `storage.Service` from the injected deps and builds the
`internal/api` server. `Run` starts the HTTP listener and blocks; on `ctx`
cancellation it triggers graceful shutdown. Ownership: `cmd/coordinator` owns the
lifecycle of the pool/backend/keystore (it created them) and closes them after
`Run` returns; `Shutdown` only stops accepting requests and drains in-flight
ones.

**`RegisterProduct` is deferred to M5, deliberately.** Adding a method to the
concrete `Coordinator` struct in M5 is a backward-compatible (minor) change, so
there is no semver "retrofit" cost to avoid by building it now — and `product/`
is explicitly `v0.x.y` unstable, so it should be shaped against nova-image's real
`RegisterRoutes`/`Migrations` needs. What M3 *does* provide as cheap
forward-compatibility: the chi router is structured so product sub-routers can be
mounted later, and the storage-core/product namespaces are reserved (below).

### Namespace reservation (cheap forward-compat)

Per `PRODUCT_MODULE_INTERFACE.md:222-238`, M3's router reserves the documented
prefixes so later milestones mount cleanly and nothing silently claims a product
path. In M3 only `/blob` and `/health` are live; the reserved prefixes
(`/api/v1`, `/fed/v1`, `/legal`, `/i`, `/v`, `/a`, `/d`, `/r`) return 404 via the
default handler. A route-shape test asserts core ownership of `/blob` + `/health`
and that the reserved prefixes are not accidentally served.

---

## Data access (sqlc)

M3 adopts sqlc for the read-path queries — the first milestone with a real query
surface (M2's jobs queue used hand-written pgx and stays as-is).

- `internal/db/queries/blobs.sql`, `internal/db/queries/collections.sql` — typed
  queries (see "Read pipeline").
- `internal/db/sqlc.yaml` — `pgx/v5` driver, `emit_interface: true` so the
  storage service depends on a generated `Querier` interface (fakeable in unit
  tests).
- `internal/db/gen/` — **committed** generated code (overrides the
  "gitignored" note in the design's repo layout). Rationale: `go build` must work
  without sqlc installed, and a CI gate catches drift.
- `tools.go` — add the sqlc generator import to pin its version (matches the
  existing goose/testcontainers anchoring pattern).
- `Makefile` — `make sqlc-generate` (runs the pinned sqlc) and `make
  codegen-check` (regenerate + `git diff --exit-code`). CI runs `codegen-check`.

The single authoritative read query joins the four tables the read path needs:

```sql
-- GetBlobForRead :one
SELECT b.cid, b.state, b.mime_type, b.envelope_version, b.encryption_key_id,
       k.wrapped_key, k.state AS key_state, k.master_key_version_id,
       m.plaintext_size, b.uploaded_at, b.product, b.owner_id
FROM blobs b
LEFT JOIN data_encryption_keys k ON k.id = b.encryption_key_id
LEFT JOIN blob_manifests       m ON m.cid = b.cid
WHERE b.cid = $1;

-- ResolveBlobVisibility :many   (collection memberships for the blob)
SELECT c.visibility, c.public_archival
FROM collection_items ci
JOIN collections c ON c.id = ci.collection_id
WHERE ci.blob_cid = $1;
```

`encryption_key_id` is nullable (NULL ⟹ `public_archival` plaintext blob, per
`DATA_MODEL.sql:283`), so the DEK join is a LEFT JOIN. Every blob has a
`blob_manifests` row (created at import); a missing manifest is a data-integrity
error → 500 (and is what the M8 `manifest_consistent` audit exists to catch).

---

## Read pipeline (`storage.Service.Get`)

```
Get(ctx, cidStr) → (*BlobResult, error):

1. cid.Decode(cidStr); parse failure → ErrBlobNotFound
2. q.GetBlobForRead(cid):  no row → ErrBlobNotFound
3. blob.state:
     active       → continue
     quarantined  → ErrBlobQuarantined
     soft_deleted → ErrBlobSoftDeleted
     tombstoned   → ErrBlobTombstoned
4. visibility := resolve(q.ResolveBlobVisibility(cid)):
     any membership 'public'   → PUBLIC
     else any 'unlisted'       → UNLISTED
     else (private-only / none)→ PRIVATE
   PRIVATE → ErrBlobAuthRequired      (no authenticator exists in M3; 401)
5. bytes:
   if encryption_key_id IS NULL (public_archival plaintext):
       reader := backend.Get(cid)              # streamed; Range allowed
       plaintextSize := m.plaintext_size
   else:
       key_state == 'shredded' → ErrKeyShredded
       perBlobKey := keystore.Unwrap(wrapped_key, master_key_version_id)
       env := readAll(backend.Get(cid))        # v1 single-shot needs whole envelope
       _, codec, err := envelope.Decode(env);  decrypt → plaintext bytes
       reader := bytes.NewReader(plaintext); Range → 416 at handler
6. return BlobResult{
       Reader, MIME: mime_type, PlaintextSize: m.plaintext_size,
       CID: cidStr, EnvelopeVersion: envelope_version,
       Visibility, Encrypted: encryption_key_id != NULL }
```

`keystore.Unwrap` failure (a master-key version no longer loaded), `backend.Get`
failure, `envelope.Decode`/`Decrypt` failure, and a missing manifest are
*infrastructure* errors → 500 (not domain sentinels). Decrypt auth failure is
treated as an integrity fault (500), surfaced later by the M8 `sample_decrypt`
audit.

### Visibility and cache policy

| Resolved visibility | bytes `/blob/{cid}` | `/blob/{cid}.json` | `Cache-Control` (200) |
|---|---|---|---|
| PUBLIC | 200 | 200 metadata | `public, max-age=31536000, immutable` |
| UNLISTED | 200 | 200 metadata | `private, max-age=300, must-revalidate` |
| PRIVATE (incl. no membership) | 401 | **404** | n/a |

**Deliberate asymmetry (do not "fix"):** private bytes return **401**
(`SignedUrlRequired`) because the openapi contract makes them signed-URL- /
bearer-recoverable (M7/M6), which inherently confirms existence; `.json` has no
signed-URL variant, so per Bug's decision it returns **404** to avoid leaking the
existence/mime/size of private blobs. Error states (soft_deleted / quarantined /
tombstoned) always imply `no-store`.

`Encrypted` blobs always carry the stratified cache header above. The openapi
`BlobBytes` example header ("Always immutable") is the common public case only;
M3 implements the stratified v2 policy from the `getBlob` description table.

---

## HTTP contract

### Endpoints

- `GET /health` → 200 `Health` `{status:"ok", version, time}`. Pure liveness; no
  DB/Kubo probe (per `openapi.yaml:73-85`). Always 200 when the server accepts
  traffic.
- `GET /blob/{cid}` → 200 decrypted bytes (or 206 for a Range on a plaintext
  `public_archival` blob). Headers below.
- `HEAD /blob/{cid}` → 200 with headers, no body. Uses `blob_manifests.plaintext_size`
  for `Content-Length` so HEAD **never** fetches from Kubo or decrypts.
- `GET /blob/{cid}.json` → 200 `Blob` metadata for anonymously-readable blobs;
  404 otherwise.

### Success headers (bytes)

`Content-Type` = `mime_type`; `Content-Length` = plaintext size (`len(plaintext)`
on GET, `blob_manifests.plaintext_size` on HEAD); `ETag` = `"<cid>"`;
`X-Nova-Cid` = cid; `X-Nova-Envelope-Version` = `envelope_version`;
`Cache-Control` per the matrix above. `X-Request-ID` echoed by middleware.

### Range handling

The handler inspects the `Range` header. Plaintext (`public_archival`) blobs are
served with `http.ServeContent` (native 206 + `Content-Range`). Encrypted blobs
with a `Range` header → **416** `RangeNotSatisfiable` (single-shot v1 is not
range-serveable; streaming AEAD is Phase 2).

### Error → status → openapi response

| storage result | status | openapi response |
|---|---|---|
| ok | 200 / 206 | `BlobBytes` / `PartialBlobBytes` |
| `ErrBlobNotFound` / invalid CID | 404 | `NotFound` |
| `ErrBlobAuthRequired` (bytes) | 401 | `SignedUrlRequired` |
| private (`.json`) | 404 | `NotFound` |
| `ErrBlobSoftDeleted` / `ErrBlobTombstoned` / `ErrKeyShredded` | 410 | `Tombstoned` |
| `ErrBlobQuarantined` | 451 | `Quarantined` |
| Range on encrypted | 416 | `RangeNotSatisfiable` |
| Kubo / unwrap / decrypt / missing-manifest | 500 | `Error` |

All non-2xx bodies use the `Error` schema `{code (snake_case), message,
request_id, details?}` (`openapi.yaml:1446-1462`). `code` examples:
`not_found`, `signed_url_required`, `gone`, `quarantined`,
`range_not_satisfiable`, `internal`. `request_id` is the middleware-assigned
`X-Request-ID`.

### `Blob` metadata (`.json`)

Populated from the read query: `cid`, `mime_type`, `byte_size` =
`blob_manifests.plaintext_size`, `uploaded_at`, `state`, `product`, `owner_id`
(nullable), and `urls{bytes:"/blob/{cid}", json:"/blob/{cid}.json"}` built as
absolute URLs from the forwarded host (`X-Forwarded-Host`/`Host`), falling back
to relative paths.

---

## Startup validation and the `nova_dev` floor

`cmd/coordinator` enforces the read-relevant subset of the design's "network
exposure floor" before serving:

- `ipfs.ValidateConfig(kuboCfg, mode)` must pass (M2 logic; refuse-to-start on
  any `KUBO_HARDENING.md` violation).
- Master key loaded (`Keystore` constructed; `Bootstrap` succeeds).
- Process is not UID 0 inside the container.
- **`auth.anonymous` gate:** the policy lives in two build-tagged files in
  `internal/auth`:
  - `anonymous_prod.go` (`//go:build !nova_dev`): `EnforceAnonymousPolicy(cfg)`
    returns a refuse-to-start error when `cfg.Auth.Anonymous` is true.
  - `anonymous_dev.go` (`//go:build nova_dev`): permits it (the dev anonymous
    management bypass).

  M3 has no protected endpoints, so the only observable effect now is: a
  production binary refuses to start with `auth.anonymous: true`; a `nova_dev`
  binary starts. M6 drops `nova_dev` from production builds once bearer auth
  exists. Startup tests cover both build variants.

Refuse-to-start prints a precise message naming the offending key and exits
non-zero (visible in `docker compose logs`).

---

## nginx (minimal dev proxy)

`docker/nginx/nova.dev.conf` is a **deliberate milestone-local slice**, not the
Phase-1 target. It is a single HTTP server block that proxies `/health` and
`/blob/` to `coordinator:9000`, forwarding `X-Request-ID`, `X-Forwarded-Host`,
and `X-Forwarded-For`. It does **not** terminate TLS, split public/admin origins,
cache, or rate-limit — those are M7/M11/M13 and will replace this file via the
wizard-rendered `nova.conf`. The config preserves the future route families
(reserved prefixes pass through and 404 at the coordinator) so later topology
work does not have to rewrite M3 routing.

The full Dockerfile + a coordinator service in `docker-compose.yml` are M13; M3
ships only this proxy config, exercised by the integration test (below).

---

## Testing strategy

### Unit

- **`storage.Service.Get`** (table-driven) against a real testcontainer Postgres
  (`internal/dbtest`) + migrations + a **fake `ipfs.Backend`**: covers blob
  states (active/quarantined/soft_deleted/tombstoned), `key_state='shredded'`,
  visibility (public/unlisted/private/none), and encrypted-vs-plaintext paths.
  Asserts the right typed error or `BlobResult`.
- **Handlers** via `httptest` against the chi router with a fake storage service:
  status codes, `Error` JSON shape, success headers, `Cache-Control` per class,
  Range→416 on encrypted, Range→206 on plaintext, `.json` 200/404 matrix, HEAD
  without body.
- **Middleware:** request-id (generate when absent, echo when present), recover
  (panic → 500 with `Error` body, no stack leak), rate-limit (bucket exhaustion →
  429). Plus `internal/ratelimit` bucket unit tests.
- **Startup floor:** `EnforceAnonymousPolicy` behavior under each build tag.

### Integration (`internal/integration/m3_read_api_test.go`)

Testcontainer Postgres + migrations + a real `EmbeddedBackend` (offline) +
`Keystore`. A seed helper performs what M4 will automate: create user +
collection (+ `collection_items`), generate a per-blob key, `Keystore.Wrap` it,
`envelope.Encrypt`, `backend.AddDeterministic`, and insert `blobs` +
`blob_manifests` + `blob_blocks` + `data_encryption_keys` rows. The coordinator
runs in-process; an **nginx testcontainer** loads `nova.dev.conf` and proxies to
the in-process coordinator via the testcontainers host gateway.

Assertions: `GET /blob/{cid}` through nginx → 200 and **byte-equal** to the
original plaintext; `GET /health` → 200; plus negative cases — quarantined→451,
soft_deleted→410, private→401, bad-cid→404, encrypted+Range→416, and a public
blob's `.json`→200 / a private blob's `.json`→404.

**Fallback** if the nginx→host-gateway wiring proves flaky in CI: the automated
test targets the coordinator directly and nginx is exercised by a separate
`make smoke`-style check. The plan records this fallback explicitly.

### Memory / size budget

v1 single-shot decryption holds the entire plaintext in memory per encrypted read
(inherent to the envelope; streaming AEAD is Phase 2). M3 documents a Phase-1
**tested-size tier** and the integration test includes a representative large
encrypted blob (target: a few MiB, matching the M2 5-MiB round-trip test) to
exercise the whole-object path. This is a documented budget, not a latency SLO
(load testing / p95 targets are Phase 5 hardening).

### CI gates added in M3

`make codegen-check` (sqlc regenerate + `git diff --exit-code`). Existing
`test`/`lint`/`schema-drift` gates continue to run.

---

## Security and privacy considerations

- **Operator-visible plaintext is intended.** Per `ARCHITECTURE_DECISIONS.md`
  Tier 1, Nova is not operator-blind E2EE; the coordinator decrypts on read. M3
  implements exactly that and nothing more.
- **No telemetry / phone-home** (Tier 1). M3 adds only **local** structured
  request logging. Source IPs are subject to `source_ip_retention_days`; request
  logs must not persist full client IPs beyond that policy (the read path does
  not write `blobs.source_ip` — that is the M4 write path's concern).
- **Existence disclosure** is minimized on `.json` (private → 404). The bytes
  path's 401-for-private is contract-mandated and accepted (signed-URL-recoverable).
- **No new Kubo exposure.** The embedded Kubo API/Gateway stay loopback-only per
  M2 `ValidateConfig`; M3 adds no new outbound or inbound Kubo surface.

---

## Risks and notes

1. **nginx → in-process coordinator over the testcontainers host gateway** is the
   one fiddly test mechanism; mitigated by the documented direct-coordinator
   fallback + `make smoke`.
2. **Whole-object decryption RAM** for large encrypted blobs (above); documented
   budget + representative test; resolved structurally by Phase-2 streaming AEAD.
3. **Manifest dependency:** the read path trusts that every blob has a
   `blob_manifests` row (M4 guarantees it). Absence → 500; caught by the M8
   `manifest_consistent` audit.
4. **`byte_size` ambiguity:** the read path uses `blob_manifests.plaintext_size`
   (unambiguous) for `Content-Length` and `Blob.byte_size`, not `blobs.byte_size`.
5. **Cache-header drift vs openapi:** M3 implements the stratified table, not the
   `BlobBytes` "always immutable" example; noted so a future oapi-codegen
   adoption (M4) reconciles the example, not the behavior.

---

## File structure

### Created in M3

| Path | Purpose |
|---|---|
| `pkg/coordinator/coordinator.go` | `Config`, `New`, `Run`, `Shutdown` |
| `pkg/coordinator/coordinator_test.go` | Lifecycle tests |
| `pkg/coordinator/storage/blob.go` | `Service`, `Get` (read core) |
| `pkg/coordinator/storage/types.go` | `BlobResult`, `Visibility`, `CacheClass` |
| `pkg/coordinator/storage/errors.go` | Domain sentinel errors |
| `pkg/coordinator/storage/blob_test.go` | Storage read tests (pg + fake backend) |
| `internal/api/server.go` | chi router + middleware stack + namespace reservation |
| `internal/api/errors.go` | `Error` JSON writer |
| `internal/api/handlers/health.go` | `/health` |
| `internal/api/handlers/blob.go` | `/blob/{cid}` GET/HEAD + `.json` |
| `internal/api/handlers/*_test.go` | Handler tests (httptest + fake service) |
| `internal/api/middleware/requestid.go` | `X-Request-ID` |
| `internal/api/middleware/recover.go` | panic → 500 |
| `internal/api/middleware/ratelimit.go` | per-IP token-bucket middleware |
| `internal/api/middleware/*_test.go` | Middleware tests |
| `internal/ratelimit/bucket.go` | Token bucket |
| `internal/ratelimit/bucket_test.go` | Bucket unit tests |
| `internal/auth/anonymous_prod.go` | `//go:build !nova_dev` floor |
| `internal/auth/anonymous_dev.go` | `//go:build nova_dev` bypass |
| `internal/auth/anonymous_test.go` | Build-tagged startup tests |
| `cmd/coordinator/main.go` (+ `wire.go`) | Wiring + startup validation + SIGTERM |
| `internal/db/queries/blobs.sql` | Read queries |
| `internal/db/queries/collections.sql` | Visibility query |
| `internal/db/sqlc.yaml` | sqlc config |
| `internal/db/gen/*.go` | Committed generated code |
| `internal/integration/m3_read_api_test.go` | nginx-fronted E2E read test |
| `docker/nginx/nova.dev.conf` | Minimal dev proxy |

### Modified in M3

| Path | Why |
|---|---|
| `go.mod` / `go.sum` | Add `github.com/go-chi/chi/v5`; sqlc generator anchor |
| `tools.go` | Anchor the pinned sqlc version |
| `Makefile` | `sqlc-generate`, `codegen-check`, coordinator build/run targets |
| `.github/workflows/ci.yml` | Add `codegen-check` gate |
| `docs/specs/openapi.yaml` | Reconciliations: split `private/unlisted` (l.112); streaming-AEAD "Phase 2" (l.117-119) |
| `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md` | Mark M3 status; amend `audit_log` to M6; link M3 plan |

---

## Cross-references

- `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md` — Phase 1
  architecture (read pipeline §, container topology §, error handling §).
- `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md` — milestone table.
- `docs/superpowers/plans/2026-05-25-phase1-m2-envelope-ipfs.md` — M2 plan (deps M3 builds on).
- `docs/specs/openapi.yaml` — `/health`, `/blob/{cid}`, `/blob/{cid}.json` contracts + schemas.
- `docs/specs/DATA_MODEL.sql` — `blobs`, `data_encryption_keys`, `collections`,
  `collection_items`, `blob_manifests` (+ migration 0004 `envelope_version`).
- `docs/specs/PRODUCT_MODULE_INTERFACE.md` — coordinator constructor + URL prefix reservations.
- `docs/specs/ENCRYPTION_ENVELOPE.md` — envelope v1 read/decrypt semantics.
- `docs/specs/KUBO_HARDENING.md` — startup validator rules enforced before serving.
- `docs/specs/ARCHITECTURE_DECISIONS.md` — Tier 1 constraints (no telemetry; operator-visible reads).
