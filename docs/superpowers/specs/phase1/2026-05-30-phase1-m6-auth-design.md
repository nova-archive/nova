# Phase 1 M6 — Local JWT Issuer + Bearer Auth Design

## Purpose and scope

M6 is the sixth Phase 1 milestone and the first of the backend-capability band
(M6–M10). It converts the coordinator from "anonymous floor in dev / refuse-to-start
in prod" into a genuinely authenticated service: a local OIDC-shaped JWT issuer, a
bearer middleware that hydrates an optional caller identity, role guards on the
privileged surface, a verify-only external-OIDC adapter, and the `novactl` CLI's first
subcommand. It also makes the production (untagged) build the exercised auth path and
resolves real `owner_id` on writes.

M1–M5 ran the write and transform paths under M3's `nova_dev` anonymous floor — every
request was unauthenticated and `owner_id` was always `NULL`. M6 removes that floor from
the production path. The coordinator already refuses to start with `auth: anonymous` in
non-`nova_dev` builds (`internal/auth/anonymous_prod.go`, T1.19); M6 supplies the real
auth that floor was standing in for.

### In scope

- **`internal/auth/password`**: argon2id hashing + constant-time verify, with a
  precomputed dummy-hash equalizer (anti-enumeration) and tuned, documented parameters.
- **`internal/auth/token`**: access-JWT claims type, EdDSA (Ed25519) sign/verify via
  `go-jose/v4`, and JWKS construction from the local signing key.
- **`internal/auth/localissuer`**: the OIDC-shaped issuer — `POST /api/v1/auth/login`,
  `/refresh`, `/logout`, `GET /api/v1/auth/jwks.json`, and `GET /api/v1/auth/config`.
  Owns access-token minting and the rotating refresh-token lifecycle.
- **`internal/auth/oidc`**: verify-only external adapter built on `coreos/go-oidc/v3`
  (OIDC discovery + thread-safe JWKS cache + verification), with a configured
  claim → role mapping.
- **`internal/auth` `Verifier` seam**: a small interface both the local issuer and the
  external adapter satisfy. The bearer middleware holds an ordered set of verifiers and
  tries each, so local and external tokens verify concurrently during migration.
- **`internal/auth/bearer`**: chi middleware — `Optional` (hydrate identity, never
  reject), `RequireAuthenticated` (401 if no identity), `RequireRole(...)` (401/403).
- **Write-path identity**: upload handlers read the optional identity and set
  `owner_id`; uploads require `uploader|moderator|operator` by default, with an operator
  opt-in (`uploads.public_uploads`) gated by T1.20 (`tos_url` required).
- **Abuse resistance**: a strict per-IP limiter on `/api/v1/auth/login` plus a global
  argon2 concurrency semaphore (bounds peak hash memory regardless of source-IP spread).
- **`cmd/novactl`**: new binary; `novactl auth login | whoami | logout` against the local
  issuer, caching credentials at `~/.config/nova/credentials.json` (0600).
- **`GET /api/v1/users/me`** (`getCurrentUser`, already in `openapi.yaml`): implemented as
  the canonical protected read that proves the middleware end-to-end.
- **`/api/v1/admin/*` protected route group**: mounted now with `RequireRole(operator|
  moderator)` so the privileged boundary exists; concrete admin endpoints arrive M7–M10.
- Unit + nginx-fronted integration tests; CI builds and exercises the production
  (untagged) binary.

### Out of scope (with the milestone that owns each)

- **`audit_log` writer + `GET /api/v1/admin/audit-log`** — **M9**. The table exists
  (`DATA_MODEL.sql`); M3 deferred its middleware to "M6", but in M6 the only privileged
  actions are auth events, and `audit_log` is for operator-action history. M6 emits
  **structured logs** for auth security events (failed login, refresh-token reuse,
  logout); the DB writer lands with the first real operator actions (takedowns) in M9.
- **Collection-management API (`/api/v1/collections*`) and authenticated metadata
  read/update (`GET/PATCH /api/v1/blobs|images/{cid}`)** — a later dedicated REST
  milestone / **M11** (when the admin SPA first consumes them). M3/M4/M5 forward-referenced
  "M6" for these; M6 deliberately keeps auth infrastructure separate from product CRUD so
  the security-critical code is reviewable in isolation.
- **Signed URLs + signing-key rotation** — **M7** (`signing_keys`, HMAC, revocation).
- **OIDC signing-key *rotation*** — not M6. M6 loads a single Ed25519 OIDC signing key;
  JWKS is `kid`-keyed so future rotation drops in. (M7 rotates the *signed-URL* HMAC keys,
  which are a separate key class.)
- **Interactive OIDC login (authorization-code + PKCE redirect/callback)** — a client
  concern. The coordinator is a resource server: it verifies bearer tokens and never
  mediates a browser auth-code exchange (this would force stateful relying-party
  session/nonce/CSRF handling). The SPA (**M11**) and a future `novactl` interactive mode
  drive PKCE themselves, discovering the IdP via `GET /api/v1/auth/config`.
- **operator.yaml loader wired into `cmd/coordinator`** — still deferred (M5 precedent).
  M6 continues the env-driven `cmd` pattern, adding the auth env knobs below.

---

## Source of truth and required doc reconciliations

1. **`docs/specs/openapi.yaml` — add the local-issuer auth surface (currently absent).**
   `/api/v1/users/me` (`getCurrentUser`) already exists and returns `#/components/schemas/User`;
   M6 *implements* it. M6 *adds* `POST /api/v1/auth/login`, `/refresh`, `/logout`,
   `GET /api/v1/auth/jwks.json`, and `GET /api/v1/auth/config`, plus the schemas
   `LoginRequest`, `TokenResponse`, `RefreshRequest`, `LogoutRequest`, `Jwks`, and
   `AuthConfig`. The `bearerAuth` security scheme description hard-codes "Authelia" — broaden
   it: the **local issuer is the Phase-1 default**; external OIDC is the operator opt-in.

2. **`docs/specs/DATA_MODEL.sql` — header fix only; the new objects are migration-only.**
   sqlc reads `internal/db/migrations/` (`internal/db/sqlc.yaml:4 → schema: "migrations"`),
   not this file, and migrations 0002–0005 (jobs, partitions, envelope_version,
   upload_sessions) were all left migration-only. `DATA_MODEL.sql` is therefore a Phase-0 v2
   baseline that has already drifted. M6 follows that precedent: `users.password_hash`,
   `users.disabled`, and `refresh_tokens` live in `0006_auth.sql` only. The fix is to the
   **header text**: replace "This file is the authoritative schema … sqlc reads the schema"
   with a statement that this is the Phase-0 v2 baseline and that `internal/db/migrations/`
   is the Phase-1 source of truth sqlc consumes.

3. **`docs/superpowers/plans/phase1/2026-05-25-phase1-single-node-mvp.md` — M6 row + exit fix.**
   Mark M6 status; link this plan. **Amend the M6 exit criterion**: the phrase "external
   OIDC mode → local-issuer endpoints return 404 **and redirects to issuer**" is
   self-contradictory (a response cannot be both 404 and a redirect). Replace with: "when
   `issuer_url` is set, local issuer endpoints return **404 `external_oidc_active`**;
   clients discover the IdP via `GET /api/v1/auth/config` and drive PKCE themselves; a token
   from the external issuer verifies through the bearer middleware."

4. **`internal/config/types.go`** — add `Auth.ClientSecretFile`, `Auth.RoleClaim`,
   `Auth.RoleMapping`, and `Uploads.PublicUploads` (comments only beyond the fields).

5. **`docs/specs/ARCHITECTURE_DECISIONS.md`** — add a Tier-2 (operator-tunable) note for
   the external-OIDC role-claim mapping; reaffirm T1.19/T1.20 are now enforced with real
   auth present, not just the refuse-to-start stub.

6. **`go.mod`** — promote `github.com/go-jose/go-jose/v4` from indirect to direct; add
   `github.com/coreos/go-oidc/v3` (Apache-2.0; sits on go-jose, so no crypto-stack
   conflict).

---

## Preconditions from M1–M5 (confirmed in committed code)

- `users` table (`DATA_MODEL.sql:148`): `id, email citext, role user_role, created_at,
  updated_at`. **No `password_hash`** → migration 0006.
- `user_role` enum: `viewer | uploader | moderator | operator`.
- `audit_log` table exists; nothing writes to it (writer deferred to M9, above).
- `owner_id` is nullable on `blobs`, `collections`, `upload_sessions`; always `NULL` today.
- `internal/auth` holds only `EnforceAnonymousPolicy` (build-tagged `anonymous_prod.go` /
  `anonymous_dev.go`). Retained; M6 adds real auth alongside.
- Router: `internal/api/server.go` `NewServer` mounts `/health`, `/blob`,
  `/api/v1/uploads|blobs|images`; the rest of `/api/v1/*` 404s. No middleware beyond
  request-id, recover, rate-limit. No `/api/v1/auth` or `/api/v1/admin`.
- `internal/ratelimit.Limiter` (`bucket.go`): concurrency-safe keyed token bucket; keyed on
  the caller-supplied key (middleware uses `X-Forwarded-For`, trustworthy only behind the
  nginx front — which is the Phase-1 topology).
- `httputil.WriteError(w, status, code, message, requestID)` writes the `Error` shape
  (`{code, message, request_id}`) with `Cache-Control: no-store`.
- `cmd/coordinator/main.go` is env-driven; calls `auth.EnforceAnonymousPolicy`. Secret
  resolver (`internal/config/secrets.go`, M1) implements the
  `NOVA_FOO → NOVA_FOO_FILE → /run/secrets/foo` chain.
- `cmd/novactl/` is an empty `.gitkeep` — no `main.go` yet.
- Dependencies present: `golang.org/x/crypto` (argon2 + ed25519 helpers; Ed25519 itself is
  stdlib `crypto/ed25519`), `go-jose/go-jose/v4` (indirect, via go-dag-jose).

---

## Architecture

```
                   Authorization: Bearer <jwt>
   client ───────────────────────────────────────────► chi router
                                                            │
                              request-id, recover, ratelimit (global)
                                                            │
                          ┌─────────────────────────────────┴───────────────┐
                          │  /blob, /i, /health        /api/v1/* subtree     │
                          │  (no auth middleware)      bearer.Optional       │
                          │                              │                   │
                          │                    hydrates Identity{user,role}  │
                          │                              │                   │
                          │            ┌─────────────────┼──────────────────┐│
                          │            │ /api/v1/users/me │ /api/v1/admin/*  ││
                          │            │ RequireAuthd     │ RequireRole(op,  ││
                          │            │                  │   moderator)     ││
                          │            │ /api/v1/uploads|blobs|images        ││
                          │            │   RequireRole(uploader+) OR public  ││
                          │            └─────────────────────────────────────┘│
                          └──────────────────────────────────────────────────┘

   bearer.Optional ──► []Verifier (ordered, tried in turn):
        localissuer.Verifier  (in-process Ed25519 pubkey; active when issuer_url empty)
        oidc.Verifier         (coreos/go-oidc; active when issuer_url set)
```

The **`Verifier` seam** is the central abstraction:

```go
// internal/auth/verifier.go
type Identity struct {
    UserID string      // sub
    Role   string      // viewer|uploader|moderator|operator
    Issuer string      // iss (local issuer URL or external IdP)
}

type Verifier interface {
    // Verify parses and validates a bearer token. It returns ErrTokenNotForMe
    // (sentinel) when the token's issuer is not this verifier's, so the
    // middleware can try the next verifier without treating it as a failure.
    Verify(ctx context.Context, raw string) (Identity, error)
}
```

The middleware iterates verifiers: a `ErrTokenNotForMe` falls through to the next; a real
validation error (bad signature, expired) short-circuits to a `401`; a success hydrates the
identity. This is what "accepts both the local issuer and external OIDC concurrently" means
in practice, while the local *minting* endpoints can still be disabled (404) under external
mode.

### Package boundaries

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/auth/password` | argon2id hash/verify, dummy equalizer | x/crypto/argon2 |
| `internal/auth/token` | access-JWT claims, EdDSA sign/verify, JWKS build | go-jose/v4 |
| `internal/auth/localissuer` | login/refresh/logout/jwks/config handlers, refresh lifecycle | token, password, db |
| `internal/auth/oidc` | external verify-only adapter, claim→role | coreos/go-oidc/v3 |
| `internal/auth/bearer` | Optional / RequireAuthenticated / RequireRole | verifier seam |
| `internal/auth` (root) | `Verifier`, `Identity`, context get/put, `EnforceAnonymousPolicy` | — |

---

## Data model additions (`internal/db/migrations/0006_auth.sql`)

Migration-only (header note in reconciliation #2). goose up/down, mirroring 0005's style.

```sql
-- 0006: local-issuer auth — password credentials + rotating refresh tokens.
-- See docs/superpowers/specs/phase1/2026-05-30-phase1-m6-auth-design.md.

ALTER TABLE users ADD COLUMN password_hash text;            -- NULL for external-OIDC users
ALTER TABLE users ADD COLUMN disabled boolean NOT NULL DEFAULT false;

CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash  bytea NOT NULL,                 -- SHA-256 of the opaque secret; never the secret
    issued_at   timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    rotated_to  uuid REFERENCES refresh_tokens (id),  -- set when this token is rotated (reuse detection)
    revoked_at  timestamptz,                    -- set on logout or family revocation
    user_agent  text,

    UNIQUE (token_hash)
);

CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);
CREATE INDEX refresh_tokens_gc_idx   ON refresh_tokens (expires_at)
    WHERE revoked_at IS NULL;
```

- `refresh_tokens` follows the OAuth 2.0 BCP for SPAs: opaque 32-byte secret (base64url),
  stored only as SHA-256, single-use with rotation. On refresh, the presented token is
  marked `rotated_to` the new token's id; presenting a token that already has `rotated_to`
  set (or `revoked_at`) is **reuse** → revoke the family and emit a structured security log.
  Phase-1 implements "family" as **all of the user's live refresh tokens** (`RevokeRefreshTokenFamily`
  is `WHERE user_id = $1 AND revoked_at IS NULL`) — coarser than per-`rotated_to`-lineage but
  strictly safer: a stolen token logs the user out of every session. (A future refinement could
  scope revocation to the compromised chain.)
- `ON DELETE CASCADE` on `user_id`: refresh tokens have no meaning without their user
  (consistent with `DATA_MODEL.sql`'s cascade convention).
- GC: a periodic sweep deletes rows past `expires_at` (reuse the coordinator's existing
  ticker pattern, alongside the upload-session GC loop).
- No `filename`/IP columns (data minimization, consistent with `upload_sessions`).

sqlc queries (`internal/db/queries/auth.sql`, regenerate `internal/db/gen`):
`GetUserByEmail`, `GetUserByID`, `CreateUser` (with hash), `SetUserPasswordHash`,
`InsertRefreshToken`, `GetRefreshTokenByHash`, `MarkRefreshTokenRotated`,
`RevokeRefreshToken`, `RevokeRefreshTokenFamily`, `DeleteExpiredRefreshTokens`.

---

## Token model

**Access token** — JWT, `alg: EdDSA` (Ed25519), 15-minute TTL.

| Claim | Value |
|---|---|
| `iss` | configured issuer (`NOVA_AUTH_ISSUER_URL` or `https://{hostname}/`) |
| `sub` | user id (uuid) |
| `aud` | `nova` |
| `iat`, `exp` | issued-at, +15 min |
| `jti` | random id |
| `role` | `viewer \| uploader \| moderator \| operator` |

Header carries `kid` = first 8 bytes of the public key, hex. Stateless: verified by
signature + `exp` + `iss`/`aud`, no DB hit on the read path.

**Refresh token** — opaque, 12-hour TTL, DB-backed, single-use rotating (above). Logout
revokes; reuse revokes the family.

**Signing key** — Ed25519, loaded via the secret resolver
`NOVA_OIDC_SIGNING_KEY → _FILE → /run/secrets/oidc-signing-key` as a hex-encoded 32-byte
seed (consistent with the master-key hex convention; `ed25519.NewKeyFromSeed`). JWKS
publishes the public half as an OKP JWK. Local mode **refuses to start** if the key is
absent (§ Startup validation).

---

## Local-issuer HTTP surface

Mounted under `/api/v1/auth` **only when `auth.issuer_url` is empty** (local mode). All
responses are `Cache-Control: no-store`.

| Route | Body → Result |
|---|---|
| `POST /api/v1/auth/login` | `{username, password}` → `TokenResponse {access_token, refresh_token, token_type:"bearer", expires_in, kid}`. argon2id verify; constant-time; generic `invalid_credentials` 401 on any failure. |
| `POST /api/v1/auth/refresh` | `{refresh_token}` → new `TokenResponse` (access + rotated refresh). Reuse/expiry → `invalid_refresh_token` 401. |
| `POST /api/v1/auth/logout` | `{refresh_token}` → `204`; revokes the presented token. |
| `GET /api/v1/auth/jwks.json` | → JWKS (public OKP key). Short-cache. |
| `GET /api/v1/auth/config` | → `AuthConfig {mode, issuer_url?, client_id?, scopes?}`. **Always present in both modes**; unauthenticated; the discovery doc clients use to drive PKCE. |

When `issuer_url` is set (external mode): `login | refresh | logout | jwks.json` return
**404 `external_oidc_active`**; only `config` responds, returning the external descriptor.
This resolves the contradictory "404 and redirect" exit phrasing — the resource server
never redirects; the client reads `config` and goes to the IdP itself.

---

## External OIDC (verify-only)

`internal/auth/oidc` wraps `coreos/go-oidc/v3`:

- On construct: `oidc.NewProvider(ctx, issuer_url)` performs discovery
  (`/.well-known/openid-configuration`) and exposes the IdP's JWKS via a keyset that
  refreshes on `kid` miss (the library handles caching, thread-safety, and stampede
  protection — the exact surface we declined to hand-roll).
- `Verify`: `provider.Verifier(&oidc.Config{ClientID: aud})` validates signature, `iss`,
  `aud`, `exp`. Then map a configured claim to role: `Auth.RoleClaim` (default `groups`)
  read from the token; `Auth.RoleMapping` translates IdP group/scope strings (e.g.
  `nova:operator → operator`). Unmapped → `viewer`.
- Tokens not issued by this IdP return `ErrTokenNotForMe` so the local verifier (if also
  active during migration) gets a turn.

`client_secret` (when needed for token introspection in future) is resolved via
`Auth.ClientSecretFile`; M6 only verifies signed JWTs, so the secret is optional in M6.

---

## Bearer middleware + guards (Approach A: optional identity + guards)

```go
// internal/auth/bearer
func Optional(vs []auth.Verifier) func(http.Handler) http.Handler   // hydrate or pass through
func RequireAuthenticated(next http.Handler) http.Handler           // 401 unauthenticated
func RequireRole(roles ...string) func(http.Handler) http.Handler   // 401 if none, 403 if insufficient
```

- `Optional` runs across the whole `/api/v1` subtree. A valid token → `Identity` in
  context; a missing/invalid token → **continue with no identity** (it never rejects, so a
  public-upload deployment and the read endpoints keep working).
- `RequireRole(operator, moderator)` guards `/api/v1/admin/*` (group mounted now; concrete
  endpoints arrive M7–M10).
- `RequireAuthenticated` guards `GET /api/v1/users/me`.
- Write path (`/api/v1/uploads`, `/api/v1/blobs`, `/api/v1/images`): `RequireRole(uploader,
  moderator, operator)` **by default**; lifted to bare `Optional` when
  `uploads.public_uploads` is true (identity still best-effort for `owner_id`).

Wiring change: `api.ServerConfig` gains the verifier set + a `PublicUploads` flag + the
issuer handlers; `NewServer` groups `/api/v1` under `bearer.Optional` and attaches guards
per subtree. `coordinator.New` builds the verifier set from injected auth deps and threads
the flag through. Read endpoints (`/blob`, `/i`, `/health`) stay outside the auth group.

---

## Write-path `owner_id` resolution + upload policy

- `handlers.UploadHandler` (tus create, multipart, image) and `upload.Store` read
  `auth.IdentityFromContext` and set `owner_id` on `upload_sessions` / `blobs` when present
  (nullable column already exists; no migration needed for this).
- **Default**: production uploads require `uploader|moderator|operator`.
- **Opt-in**: `uploads.public_uploads: true` allows anonymous writes (owner_id `NULL`).
  Per **T1.20**, the coordinator **refuses to start** with `public_uploads: true` and an
  empty `tos_url`. New field `Uploads.PublicUploads`; env knob `NOVA_PUBLIC_UPLOADS`.

---

## Abuse resistance

Two independent issues, two layered mitigations:

1. **argon2id CPU/memory exhaustion (DoS).** At `m=64 MiB`, N concurrent logins allocate
   `N × 64 MiB`; a burst would OOM the node. Mitigations:
   - **Per-IP login limiter**: a dedicated `ratelimit.Limiter` instance applied only to
     `POST /api/v1/auth/login`, tuned strict (burst ~5, refill ~5/min). Keyed on the
     trusted-proxy `X-Forwarded-For` (valid behind the nginx front).
   - **Global argon2 concurrency semaphore**: a buffered channel (capacity e.g.
     `min(NumCPU, 4)`) wraps every argon2 computation (real *and* dummy). Over-capacity
     logins get `503` (`Retry-After`) instead of allocating. This is the bound that holds
     under a **distributed** attack across many IPs, which per-IP limiting cannot stop.
     Peak hash memory is capped at `cap × 64 MiB`.

2. **Username enumeration via timing.** A fast "user not found → 401" vs. a slow
   "argon2 verify → 401" leaks account existence. Mitigation: the login handler runs the
   same argon2 path on **every** failure branch — user-not-found, `disabled = true`, and
   `password_hash IS NULL` (external user attempting local login) all verify the submitted
   password against a process-static **precomputed dummy hash**, then return an identical
   `invalid_credentials` 401. (The dummy verify also passes through the semaphore, so the
   timing/resource profile matches the real path.)

---

## Configuration additions (`internal/config`)

```go
type Auth struct {
    IssuerURL        string            // existing; empty ⇒ local issuer
    ClientID         string            // existing
    ClientSecretFile string            // NEW (external; optional in M6)
    Scopes           []string          // existing
    JWKSCacheTTL     int               // existing
    RoleClaim        string            // NEW; default "groups"
    RoleMapping      map[string]string // NEW; IdP group/scope → nova role
    Paranoid         bool              // existing
    Anonymous        bool              // existing (dev-only floor)
}

type Uploads struct {
    // ... existing fields ...
    PublicUploads bool                 // NEW; T1.20-gated
}
```

`cmd/coordinator` env knobs added: `NOVA_OIDC_SIGNING_KEY` (+ `_FILE` / secret), 
`NOVA_AUTH_ISSUER_URL`, `NOVA_AUTH_CLIENT_ID`, `NOVA_PUBLIC_UPLOADS`. The signing key and
verifier set are built in `run()` and injected into `coordinator.New`.

---

## Startup validation changes

- **Local mode + no OIDC signing key** → refuse to start (clear message naming the env
  key), mirroring the master-key floor.
- **`public_uploads: true` + empty `tos_url`** → refuse to start (T1.20).
- `EnforceAnonymousPolicy` unchanged (T1.19 floor remains; anonymous is `nova_dev`-only).
- External mode skips the signing-key requirement (no local minting).

---

## HTTP contract

### Routes added / implemented

| Method | Path | Auth | Notes |
|---|---|---|---|
| POST | `/api/v1/auth/login` | none | local mode only; 404 in external |
| POST | `/api/v1/auth/refresh` | none | local mode only |
| POST | `/api/v1/auth/logout` | none | local mode only |
| GET | `/api/v1/auth/jwks.json` | none | local mode only |
| GET | `/api/v1/auth/config` | none | both modes |
| GET | `/api/v1/users/me` | RequireAuthenticated | implements `getCurrentUser` → `User` |
| (group) | `/api/v1/admin/*` | RequireRole(operator,moderator) | boundary only; endpoints M7–M10 |

### Error → status

| Condition | Status | `code` |
|---|---|---|
| missing OR invalid OR expired bearer on a guarded route | 401 + `WWW-Authenticate: Bearer` | `unauthenticated` |
| valid token, role too low | 403 | `forbidden` |
| external-OIDC verification temporarily unavailable (IdP discovery pending) | 503 + `Retry-After` | `auth_unavailable` |
| login: any credential failure | 401 | `invalid_credentials` |
| refresh: expired/reused/unknown | 401 | `invalid_refresh_token` |
| local auth endpoint under external mode | 404 | `external_oidc_active` |
| `/api/v1/users/me` subject is not a uuid | 401 | `invalid_token` |
| login over per-IP or semaphore limit | 503 + `Retry-After` (or 429 for the per-IP bucket) | `server_busy` |

**Fail-open `Optional`, fail-closed guards (implemented).** `bearer.Optional`
never rejects: a present-but-invalid/expired/foreign token simply does not
hydrate an identity, so the request proceeds anonymously on *unguarded* routes
(public reads, and uploads under `public_uploads`). On a *guarded* route the
guard then sees no identity and returns a single generic `401 unauthenticated`
— it deliberately does **not** distinguish bad-signature vs. expired (no
verification oracle, and clients refresh-on-any-401 regardless). The earlier
draft's separate `token_expired` / `invalid_token` guard codes are therefore
**not** surfaced by the guards; `invalid_token` survives only on the
`/users/me` non-uuid-subject path above.

---

## `novactl` CLI

New `cmd/novactl/main.go` (the binary scaffolded since M1). Phase-1 subcommands:

- `novactl auth login [--url <base>] [--username <u>]` — prompts for the password
  (no echo), POSTs `/api/v1/auth/login`, writes `~/.config/nova/credentials.json` (0600)
  with the token pair + base URL + `kid`.
- `novactl auth whoami` — GETs `/api/v1/users/me` with the cached token; auto-refreshes on
  401 if the refresh token is live.
- `novactl auth logout` — POSTs `/api/v1/auth/logout`, clears the cached creds.

External mode: `login` reads `/api/v1/auth/config`, prints the IdP URL, and instructs the
operator to obtain a token from the IdP (interactive PKCE is deferred — out of scope).
`novactl setup` (M13) is unrelated and not added here.

---

## Dropping the `nova_dev` floor as the production path

M6 does not delete the `nova_dev` build tag — it remains a local-dev convenience for M7–M10
work (anonymous bypass on loopback). What changes:

- The default/production build (no `-tags nova_dev`) now has a **fully functional** auth
  path, not just the refuse-to-start guard.
- CI builds and tests the **untagged** binary, and the M6 integration test authenticates
  for real.
- The M3-era statement "the rest of `/api/v1/*` stays 404 until M6" is realized: M6 mounts
  the `/api/v1` auth group and the admin boundary.

---

## Testing strategy

### Unit

- `password`: hash→verify round-trip; wrong password rejects; dummy-hash verify path takes
  comparable time and never panics on `NULL`/empty; encoded-hash parsing.
- `token`: sign→verify; tampered signature rejects; expired `exp` rejects; wrong `aud`/`iss`
  rejects; unknown `kid` rejects; JWKS round-trips through go-jose.
- `localissuer`: refresh rotation issues a new pair and marks `rotated_to`; reusing a
  rotated token revokes the family; logout revokes; external-mode handlers 404.
- `oidc`: verify a token minted against a stub JWKS; claim→role mapping incl. unmapped →
  viewer; foreign-issuer token returns `ErrTokenNotForMe`.
- `bearer`: guard matrix — no token / valid / expired / insufficient role → expected
  status, for `Optional`, `RequireAuthenticated`, `RequireRole`.

### Integration (`internal/integration/m6_auth_test.go`, nginx-fronted, testcontainers)

End-to-end against the production (untagged) coordinator behind an nginx testcontainer:

1. Bootstrap keystore + Ed25519 signing key; create an `operator` user (argon2id).
2. `POST /api/v1/auth/login` → token pair.
3. `GET /api/v1/users/me` with the access token → 200 + correct `User`.
4. Hit the `/api/v1/admin/*` boundary: operator token → not-404 boundary behavior;
   no token → 401; a `viewer` token → 403.
5. Forge/await an expired access token → 401 `unauthenticated` (guards don't distinguish expiry).
6. `POST /api/v1/auth/refresh` → new pair succeeds; reusing the old refresh → family
   revoked, 401.
7. `POST /api/v1/auth/logout`; subsequent refresh → 401.
8. Upload with no token (default policy) → 401; with an `uploader` token → 200 and
   `blobs.owner_id` set.
9. External-OIDC mode (configure `issuer_url` to a stub IdP): `login` → 404
   `external_oidc_active`; `config` → external descriptor; a token minted by the stub IdP
   verifies through the middleware on `/api/v1/users/me`.
10. Refuse-to-start assertions: `auth: anonymous` (untagged) refuses (T1.19, re-assert);
    `public_uploads: true` + empty `tos_url` refuses (T1.20); local mode + no signing key
    refuses.

### CI

- Build and unit-test the untagged binary (production path).
- The integration test is `-short`-skippable like M2–M5; full run in the integration job.

---

## Security and privacy considerations

- **argon2id** params documented and tuned (`t=3, m=64 MiB, p=2` as the starting point,
  benchmarked on the dev host); encoded as the standard `$argon2id$...` string so params
  travel with the hash.
- **No secrets logged**: passwords, raw refresh tokens, and access tokens never appear in
  logs or errors. Structured security logs record *events* (failed login w/ source, refresh
  reuse, logout) with user id, not credentials.
- **Constant-time** credential comparison; uniform failure path (anti-enumeration, above).
- **Refresh hygiene**: short access TTL + rotation + reuse-triggered family revocation;
  DB stores only SHA-256 of the opaque secret, so a DB dump yields no usable sessions.
- **JWKS fetch** (external mode) goes over the operator's trust path with the library's
  cache + size bounds; M6 does not fetch arbitrary attacker-controlled issuers (issuer is
  operator-configured).
- **Paranoid mode** unaffected: no new IP retention (the per-IP limiter key is in-memory
  and ephemeral); auth security events go to structured logs, not the `audit_log` table
  (writer deferred to M9), so the privacy posture is unchanged.
- **`credentials.json`** written `0600`; the CLI refuses to write world/group-readable.

---

## Risks and notes

- **Single OIDC signing key, no rotation in M6.** Accepted: JWKS is `kid`-keyed, so a
  future rotation milestone adds a second key without disturbing verification. Compromise of
  the signing key is a master-key-class event (process-resident secret) — same trust
  assumption as `NOVA_MASTER_KEY`.
- **Per-IP limiter trusts `X-Forwarded-For`.** Only sound behind the nginx front (Phase-1
  topology). The global semaphore is the source-independent backstop; documented as such.
  The limiter's unbounded key-map TODO (from M3) is inherited, not worsened.
- **`/api/v1/admin/*` is an empty guarded group in M6.** Intentional: it establishes the
  boundary and is exercised by the integration test's 401/403 matrix even before M7 adds
  endpoints. A bare guarded group returns the chi NotFound for unknown subpaths *after*
  passing the guard, which is the correct ordering (authn/authz before route resolution).
- **External-mode role mapping is operator config.** A misconfigured `RoleMapping` could
  over- or under-grant; documented in the operator notes, with `viewer` as the safe default
  for unmapped groups.
- **go-oidc network dependency at startup.** Discovery hits the IdP on construct; if the IdP
  is down at boot in external mode, construction retries/back-off is handled by deferring the
  first discovery to first-use where the library supports it, else surfaced as a clear
  startup error. (Local mode has no such dependency.)

---

## File structure

### Created in M6

```
internal/auth/verifier.go                    Verifier seam + Identity + context get/put
internal/auth/password/password.go           argon2id hash/verify + dummy equalizer
internal/auth/password/password_test.go
internal/auth/token/token.go                 access-JWT claims, EdDSA sign/verify, JWKS
internal/auth/token/token_test.go
internal/auth/localissuer/issuer.go          login/refresh/logout/jwks/config handlers
internal/auth/localissuer/refresh.go         rotating refresh lifecycle + reuse detection
internal/auth/localissuer/issuer_test.go
internal/auth/oidc/verifier.go               coreos/go-oidc verify-only adapter
internal/auth/oidc/verifier_test.go
internal/auth/bearer/bearer.go               Optional / RequireAuthenticated / RequireRole
internal/auth/bearer/bearer_test.go
internal/db/migrations/0006_auth.sql         password_hash + disabled + refresh_tokens
internal/db/queries/auth.sql                 user + refresh-token queries (sqlc)
cmd/novactl/main.go                          auth login | whoami | logout
internal/integration/m6_auth_test.go         end-to-end auth through nginx
```

### Modified in M6

```
internal/db/gen/*                            regenerated from auth.sql
internal/api/server.go                       /api/v1 auth group + guards + users/me + admin boundary
pkg/coordinator/coordinator.go               build/inject verifier set; thread PublicUploads + issuer
cmd/coordinator/main.go                      load signing key; build auth; env knobs; startup checks
internal/config/types.go                     Auth.{ClientSecretFile,RoleClaim,RoleMapping}; Uploads.PublicUploads
internal/api/handlers/upload.go              owner_id from identity; upload policy
internal/upload/store.go                     persist owner_id on session/blob
docker/nginx/nova.dev.conf                   proxy /api/v1/auth, /api/v1/users/me, /api/v1/admin
docs/specs/openapi.yaml                      add /api/v1/auth/* paths + schemas; broaden bearerAuth desc
docs/specs/DATA_MODEL.sql                    header fix (Phase-0 baseline; migrations are source of truth)
docs/superpowers/plans/phase1/2026-05-25-phase1-single-node-mvp.md   M6 status + exit-criterion amendment
docs/specs/ARCHITECTURE_DECISIONS.md         external-OIDC role-mapping note
go.mod / go.sum                              go-jose/v4 direct; add coreos/go-oidc/v3
```

---

## Cross-references

- `docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md` § "Authentication
  architecture", § "Onboarding wizard" (Steps 4–5).
- `docs/superpowers/plans/phase1/2026-05-25-phase1-single-node-mvp.md` § M6.
- `docs/specs/openapi.yaml` (`bearerAuth`, `getCurrentUser`, `User`, `Error`).
- `docs/specs/DATA_MODEL.sql` (`users`, `user_role`, `audit_log`).
- `docs/specs/ARCHITECTURE_DECISIONS.md` T1.19, T1.20, T1.21; Tier-3 operator-SSO freedom.
- `docs/THREAT_MODEL.md` § F (anonymous-management refusal), boundary ③ (secret resolver).
- `docs/specs/SIGNED_URL_FORMAT.md` (M7; distinct key class from the OIDC signing key).
