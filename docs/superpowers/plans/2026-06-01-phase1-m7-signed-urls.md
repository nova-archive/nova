# M7 Signed URLs + Signing-Key Rotation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the signed-URL HMAC verifier (`internal/auth/signedurl`), signing-key rotation with a grace window, structured `(kind, value)` revocation, and operator-side minting — and wire the verifier as a fail-through read-path Guard that unlocks private blobs on `/blob/{cid}` and `/i/*`.

**Architecture:** A `Guard` middleware engages only when a request carries signed-URL params (`sig|exp|aud|kid`); otherwise it passes through (public reads anonymous, private reads still 401). On engagement it runs the normative six-step `Verify` (schema → kid → revocation → expiry → HMAC → audience), constant-time, 0 s skew; on success it sets a request-scoped read grant in the context that `storage.Resolve` honours to bypass the private gate; on any failure it returns `403 invalid_signature`. Signing keys live in `signing_keys` (wrapped under the master key via the existing keystore); a `KeySource` resolves keys by `kid` (active or within-grace retired) behind a short TTL cache; a `Revocations` set loads `signed_url_revocations` with a 30 s refresh + in-process invalidation. Rotation/revocation/minting are admin endpoints (rotate operator-only; revoke/sign operator+moderator). The first key auto-bootstraps on boot; a GC sweep shreds past-grace keys.

**Tech Stack:** Go 1.22, stdlib `crypto/hmac` + `crypto/sha256` + `crypto/subtle` + `crypto/rand` + `encoding/base64`, pgx/v5, sqlc, chi, testcontainers-go (Postgres + nginx). No new third-party dependencies.

**Authoritative spec:** `docs/superpowers/specs/2026-06-01-phase1-m7-signed-urls-design.md` (and the normative `docs/specs/SIGNED_URL_FORMAT.md`).

---

## File Structure

**Created:**
```
internal/auth/signedurl/signedurl.go          Canonical, Sign, Verify (six-step), Decision, VerifyInput
internal/auth/signedurl/signedurl_test.go
internal/auth/signedurl/keysource.go          KeySource: ByKID/Active + keystore.Unwrap + TTL cache + Invalidate
internal/auth/signedurl/keysource_test.go
internal/auth/signedurl/revocations.go        Revocations: load/refresh/Invalidate/IsRevoked
internal/auth/signedurl/revocations_test.go
internal/auth/signedurl/guard.go              chi middleware: detect params → Verify → grant or 403
internal/auth/signedurl/guard_test.go
internal/auth/signedurl/mint.go               Mint(path, ttl, aud) → signed URL
internal/auth/signedurl/mint_test.go
internal/auth/signedurl/testdata/vectors.txt  authoritative HMAC test vectors
internal/db/queries/signedurl.sql             signing-key + revocation queries (sqlc)
internal/api/handlers/signing_admin.go        rotate-signing, revoke, sign handlers
internal/api/handlers/signing_admin_test.go
pkg/coordinator/storage/grant.go              WithReadAuthz / readAuthorized ctx helpers
internal/integration/m7_signed_urls_test.go   end-to-end through nginx
```

**Modified:**
```
internal/db/gen/*                             regenerated from signedurl.sql
pkg/coordinator/storage/blob.go               Resolve: private gate honors readAuthorized(ctx)
internal/api/server.go                        mount admin signed-URL routes; wrap /blob with Guard; operator-only rotate
pkg/coordinator/coordinator.go                build KeySource+Revocations+Guard; wrap /i/*; bootstrap; shred; readyz; ServerConfig
cmd/coordinator/main.go                       bootstrap first signing key; NOVA_SIGNED_URL_* env knobs
cmd/novactl/main.go                           signed-url sign subcommand + usage
internal/config/types.go                      SignedURLs section
docs/specs/DATA_MODEL.sql                     key_state comment reconciliation
docs/specs/SIGNED_URL_FORMAT.md               fill test vectors; flip M7 status note
docs/specs/openapi.yaml                       add /signed-urls/sign; reconcile rotate/revoke; invalid_signature codes
docs/ROADMAP.md                               M7 status + mint endpoint + tag
docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md   M7 row + link
```

**Build/test commands** (repo conventions): `go build ./...`, `go test ./pkg/... ./internal/... ./nova-image/... -short`, `go test ./internal/db/gen/...` after regen (codegen-check), integration via `go test ./internal/integration/ -run M7 -v`. Per the gofmt-skew note, run `gofmt -w` only on files you create/modify; do not reformat pre-existing files.

**Branch:** `m7-signed-urls` (one branch for the milestone; finish with a local fast-forward merge + annotated tag `m7-signed-urls`, no remote push).

---

## Task 0: sqlc queries + regenerate

**Files:**
- Create: `internal/db/queries/signedurl.sql`
- Modify: `internal/db/gen/*` (regen)

- [ ] **Step 1: Write `signedurl.sql`.** No schema change — `signing_keys` and `signed_url_revocations` already exist (`0001_init.sql`).

```sql
-- name: GetActiveSigningKey :one
SELECT kid, wrapped_key, master_key_version_id, state, retire_after
FROM signing_keys
WHERE state = 'active'
ORDER BY active_from DESC
LIMIT 1;

-- name: GetSigningKeyByKID :one
SELECT kid, wrapped_key, master_key_version_id, state, retire_after
FROM signing_keys
WHERE kid = $1;

-- name: InsertSigningKey :exec
INSERT INTO signing_keys (kid, algorithm, wrapped_key, master_key_version_id, state, active_from)
VALUES ($1, 'HMAC-SHA256', $2, $3, 'active', now());

-- name: RetirePriorActiveSigningKey :exec
UPDATE signing_keys
SET state = 'retired', retire_after = $1
WHERE state = 'active' AND kid <> $2;

-- name: ShredExpiredRetiredSigningKeys :exec
UPDATE signing_keys
SET state = 'shredded', wrapped_key = $1
WHERE state = 'retired' AND retire_after <= now();

-- name: CountActiveSigningKeys :one
SELECT count(*) FROM signing_keys WHERE state = 'active';

-- name: ListRevocations :many
SELECT kind, value FROM signed_url_revocations;

-- name: InsertRevocation :exec
INSERT INTO signed_url_revocations (kind, value)
VALUES ($1, $2)
ON CONFLICT (kind, value) DO NOTHING;
```

`RetirePriorActiveSigningKey` takes a precomputed `retire_after timestamptz` (compute `now()+grace` in Go inside the rotation tx, so the value is stable across the two statements). `ShredExpiredRetiredSigningKeys` takes the 72-byte zero `wrapped_key` as a param.

- [ ] **Step 2: Regenerate + codegen-check.**

Run: `(cd internal/db && sqlc generate) && go build ./...`
Expected: new methods on `gen.Queries`; build OK. Read `internal/db/gen/models.go` to confirm the emitted types for `wrapped_key` (`[]byte`), `master_key_version_id` (`uuid.UUID`/`pgtype.UUID`), `retire_after` (`pgtype.Timestamptz`), `state` (string or a generated enum) and adapt later tasks accordingly.

- [ ] **Step 3: Commit.**

```bash
gofmt -w internal/db/gen/  # only the regenerated files
git add internal/db/queries/signedurl.sql internal/db/gen
git commit -m "feat(db): signing-key + signed-url-revocation queries (sqlc)"
```

---

## Task 1: signedurl core — Canonical, Sign, Verify, vectors

**Files:**
- Create: `internal/auth/signedurl/signedurl.go`, `internal/auth/signedurl/signedurl_test.go`, `internal/auth/signedurl/testdata/vectors.txt`

- [ ] **Step 1: Write failing tests** — the spec's worked example canonical string, a known-key sig, and the per-code `Verify` table (using fake KeySource/Revocations + a fixed `Now`).

```go
func TestCanonicalAndSign(t *testing.T) {
    c := signedurl.Canonical("/blob/bafy/0", 0, "", "")   // spec test-vector shape
    require.Equal(t, "/blob/bafy/0\n0\n\n", c)
    key := []byte("test-key-do-not-use-in-production")     // 33 bytes, per spec
    sig := signedurl.Sign(key, c)
    require.NotContains(t, sig, "=")                        // base64url, no padding
}
```

- [ ] **Step 2: Run — expect FAIL** (`undefined: signedurl.Canonical`). `go test ./internal/auth/signedurl/`.

- [ ] **Step 3: Implement `signedurl.go`.** Canonical + Sign exactly as the design; `Verify(ctx, VerifyInput) Decision` runs the six steps against injected `KeySource`, `Revocations`, and `in.Now`. Define the failure-code constants (`signature_missing_param`, `signature_unknown_kid`, `signature_revoked`, `signature_expired`, `signature_invalid`, `signature_aud_mismatch`). Use `crypto/subtle.ConstantTimeCompare` for step 5; parse the CID from the path (leading segment after `/blob/` or `/i/`) for step 3; parse `Origin` (else `Referer`) to `scheme://host[:port]` for step 6. Equalise the failure path so the six modes are time-indistinguishable (always compute the HMAC before returning, even on an earlier miss, OR run the comparison against a dummy key — document the chosen approach).

- [ ] **Step 4: Generate `testdata/vectors.txt`** with a small program/test helper: rows of `canonical | kid | key(hex) | sig(b64url)` covering the spec's worked example and a few private-blob paths. The same vectors back-fill `SIGNED_URL_FORMAT.md` in Task 13. Add a test that re-derives every vector’s sig and asserts equality.

- [ ] **Step 5: Run — expect PASS; gofmt; commit.**

```bash
go test ./internal/auth/signedurl/ -run 'Canonical|Sign|Vectors|Verify'
gofmt -w internal/auth/signedurl/signedurl.go internal/auth/signedurl/signedurl_test.go
git add internal/auth/signedurl/signedurl.go internal/auth/signedurl/signedurl_test.go internal/auth/signedurl/testdata/vectors.txt
git commit -m "feat(signedurl): canonical string + HMAC sign/verify (six-step) + test vectors"
```

---

## Task 2: KeySource + Revocations (DB-backed, cached)

**Files:**
- Create: `internal/auth/signedurl/keysource.go`(+`_test.go`), `internal/auth/signedurl/revocations.go`(+`_test.go`)

- [ ] **Step 1: Write failing tests** against a Postgres testcontainer (mirror M6's DB test harness): seed `master_key_versions` + `signing_keys` rows; assert `ByKID` returns an active key and a within-grace retired key, rejects a past-grace retired and a shredded; `Active` returns the newest active; `Invalidate` forces a reload. For `Revocations`: insert rows, assert `IsRevoked` per kind incl. `path_prefix` hit/miss; `refresh` picks up a new row; `Invalidate` is immediate.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement.**
  - `KeySource{q *gen.Queries, ks *envelope.Keystore, ttl time.Duration}` with a small `map[string]cachedKey` (kid → unwrapped secret + state + retire_after + fetched-at). `ByKID(ctx, kid)` applies the grace rule in Go (active, or retired with `retire_after > now`), else returns `errUnknownKid`; on a cache miss it loads via `GetSigningKeyByKID` and `ks.Unwrap(ctx, wrapped, masterVersionID)`. `Active(ctx)` loads `GetActiveSigningKey`, unwraps, caches. `Invalidate()` clears the cache (called after rotate).
  - `Revocations{q, refresh time.Duration}` holds an atomic snapshot (`cid`/`aud`/`kid` sets + a `path_prefix` slice). `Load(ctx)` runs `ListRevocations`; a goroutine refreshes every `refresh`; `Invalidate(ctx)` reloads now (called after revoke). `IsRevoked(cid, aud, kid, path)` checks the three sets + prefix scan.

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(signedurl): cached KeySource + Revocations over signing_keys/signed_url_revocations`).

---

## Task 3: storage read-grant + Resolve bypass

**Files:**
- Create: `pkg/coordinator/storage/grant.go`
- Modify: `pkg/coordinator/storage/blob.go`

- [ ] **Step 1: Write failing test** (`storage/grant_test.go` or extend `blob_test.go`): a private blob `Resolve` returns `ErrBlobAuthRequired` on a bare ctx and a populated `*BlobView` under `WithReadAuthz`; a public blob resolves either way.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `grant.go`** (exact code in the design § "Read-path integration") and change the one branch in `Resolve`:

```go
visibility := resolveVisibility(vis)
if visibility == VisibilityPrivate && !readAuthorized(ctx) {
    return nil, ErrBlobAuthRequired
}
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(storage): request-scoped read grant unlocks private Resolve`).

---

## Task 4: Guard middleware

**Files:**
- Create: `internal/auth/signedurl/guard.go`(+`_test.go`)

- [ ] **Step 1: Write failing test** (httptest): no sig params → `next` called, ctx **not** authorized; valid sig (fake KeySource with a known key) → `next` called, `storage.readAuthorized(ctx)` true; tampered/expired/revoked/wrong-aud → 403 with the right specific `signature_*` `code`, `next` **not** called.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `Guard`.** `func (v *Verifier) Guard(next http.Handler) http.Handler`: if none of `sig|exp|aud|kid` present → `next.ServeHTTP`. Else build `VerifyInput` from `r` (`r.URL.Path`, `r.URL.Query()`, `Origin`/`Referer`, `time.Now()`), call `Verify`; on `OK` → `next.ServeHTTP(w, r.WithContext(storage.WithReadAuthz(r.Context())))`; else `httputil.WriteError(w, 403, code, msg, rid)` where `code` is the specific `signature_*` value from the `Decision`, and a structured log line (kid/aud/cid + `code`; never the sig). Time-equalise the failure responses.

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(signedurl): fail-through Guard middleware (verify → grant or 403)`).

---

## Task 5: first-key bootstrap

**Files:**
- Modify: `pkg/coordinator/coordinator.go` (or a small `signedurl.EnsureActiveKey` helper), `cmd/coordinator/main.go`

- [ ] **Step 1: Write failing test** (DB testcontainer): with an empty `signing_keys` table, `EnsureActiveKey(ctx, q, ks)` inserts exactly one active row whose `wrapped_key` unwraps to 32 bytes; a second call is a no-op (`CountActiveSigningKeys` stays 1).

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `EnsureActiveKey`** in `signedurl` (kept with the package that owns key minting): if `CountActiveSigningKeys == 0`, `crypto/rand` 32 bytes → `ks.Wrap` → `InsertSigningKey(genKID(), wrapped, masterVersionID)`. `genKID()` = `"k_" + base32(rand 10 bytes)` (lower, no pad). Call it from `cmd/coordinator/main.go` after `ks.Bootstrap(ctx)`, before serving.

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(signedurl): auto-bootstrap the first active signing key on boot`).

---

## Task 6: rotate-signing handler (operator)

**Files:**
- Create: `internal/api/handlers/signing_admin.go`(+`_test.go`)

- [ ] **Step 1: Write failing test**: `POST` with `{grace_seconds: 2}` → 201 `{kid, grace_expires_at}`; the table now has one new active + the prior active flipped to `retired` with `retire_after ≈ now+2s`; the KeySource cache was invalidated. Missing/zero `grace_seconds` uses the configured default.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `RotateSigning`.** In a single tx (`q.WithTx`): generate secret → `ks.Wrap` → `InsertSigningKey(newKID, wrapped, mvID)`; compute `retireAfter = now + grace`; `RetirePriorActiveSigningKey(retireAfter, newKID)`. Commit, then `keySource.Invalidate()`. Emit a structured `signing-key rotated` log (kid, grace_expires_at; no secret). Return 201.

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(api): POST /admin/keys/rotate-signing with grace window`).

---

## Task 7: signed-urls/revoke handler (operator+moderator)

**Files:**
- Modify: `internal/api/handlers/signing_admin.go`(+`_test.go`)

- [ ] **Step 1: Write failing test**: `{kind:"cid", value:"bafy…"}` → 201 `{kind, value, revoked_at}`; row present; `revocations.Invalidate` called; duplicate `(kind,value)` → still 201 (idempotent); `kind:"bogus"` → 400 `invalid_kind`.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `RevokeSignedURL`.** Validate `kind ∈ {cid,aud,kid,path_prefix}` (else 400 `invalid_kind`); `InsertRevocation(kind, value)`; `revocations.Invalidate(ctx)`; structured log (`signed-url revoked`, kind/value). Return 201.

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(api): POST /admin/signed-urls/revoke (kind,value) + cache invalidation`).

---

## Task 8: signed-urls/sign (mint) handler + novactl

**Files:**
- Modify: `internal/api/handlers/signing_admin.go`(+`_test.go`); create `internal/auth/signedurl/mint.go`(+`_test.go`)

- [ ] **Step 1: Write failing tests**: `Mint` clamps ttl to `[1,max]`, rejects a non-content path; the handler `POST {path:"/blob/{cid}", ttl_seconds:300, aud:"https://e.example"}` → 201 `{url, kid, exp}` and the returned URL re-verifies through `Verify`; a `path:"/api/v1/whatever"` → 400 `invalid_request`.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement `Mint(ctx, path, ttl, aud)`** (design § "Minting") using `KeySource.Active`; and the `SignSignedURL` handler that validates input, clamps ttl to `max_ttl_seconds`, calls `Mint`, returns 201. Structured log records `signed-url minted` (path/aud/kid/exp; never the sig).

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(api): POST /admin/signed-urls/sign (server-side mint)`).

---

## Task 9: novactl `signed-url sign` subcommand

**Files:**
- Modify: `cmd/novactl/main.go`

- [ ] **Step 1: Write failing test** (or a CLI smoke in the integration task): `novactl signed-url sign --path … --ttl … --aud …` POSTs `/api/v1/admin/signed-urls/sign` with the cached bearer token and prints the `url`.

- [ ] **Step 2: Implement** a `signed-url` branch in the `os.Args` dispatch + `usage()` line (`usage: novactl <auth …|signed-url sign --path P --ttl N --aud O>`), reusing the M6 credentials cache + HTTP client. On 401, surface "run `novactl auth login`".

- [ ] **Step 3: Run — expect build + help OK; gofmt; commit** (`feat(novactl): signed-url sign subcommand`).

---

## Task 10: shred sweep + readyz probe

**Files:**
- Modify: `pkg/coordinator/coordinator.go`

- [ ] **Step 1: Write failing test**: drive `gcLoop`'s sweep (or the extracted sweep func) — a retired-past-grace key becomes `shredded` with a 72-byte zero `wrapped_key`; a within-grace retired key is untouched. `/readyz` reports a `signing_keys` check that is `ok` with ≥1 active key and fails with none.

- [ ] **Step 2: Run — expect FAIL.**

- [ ] **Step 3: Implement.** In `gcLoop`, add `q.ShredExpiredRetiredSigningKeys(ctx, make([]byte, 72))`. In `New`, when `pool != nil`, append a `ReadyCheck{Name:"signing_keys", Fn: func(ctx) error { n,_ := q.CountActiveSigningKeys(ctx); if n==0 { return errors.New("no active signing key") }; return nil }}`.

- [ ] **Step 4: Run — expect PASS; gofmt; commit** (`feat(coordinator): shred past-grace signing keys + signing_keys readiness probe`).

---

## Task 11: wiring — Guard on /blob + /i/*, config, ServerConfig, cmd env

**Files:**
- Modify: `internal/config/types.go`, `internal/api/server.go`, `pkg/coordinator/coordinator.go`, `cmd/coordinator/main.go`

- [ ] **Step 1: Config** — add the `SignedURLs` struct (design § "Configuration additions"); thread `coordinator.Config.SignedURLs` (grace/refresh/key-ttl/max-ttl durations) and map `NOVA_SIGNED_URL_*` env knobs in `cmd/coordinator/main.go` with the default fallbacks.

- [ ] **Step 2: Build deps in `coordinator.New`** (when `pool != nil && ks != nil`): construct `KeySource`, `Revocations` (start its refresh goroutine in `Run`/under the lifecycle ctx), and the `Verifier`/`Guard`. Stash the `Guard` and the handler deps on the `Coordinator`.

- [ ] **Step 3: Mount `/blob` Guard** — add `SignedURLGuard func(http.Handler) http.Handler` to `api.ServerConfig`; in `server.go` apply `r.With(cfg.SignedURLGuard)` to the `/blob/{cid}` GET/HEAD (+ `.json`) routes when non-nil.

- [ ] **Step 4: Mount `/i/*` Guard** — in `RegisterProduct`, mount product routes under a group carrying the Guard: `c.mux.Group(func(r chi.Router){ if c.signedURLGuard != nil { r.Use(c.signedURLGuard) }; p.RegisterRoutes(r) })`. Keep the reserved-prefix probe.

- [ ] **Step 5: Mount admin routes** — in `server.go`, inside `/api/v1/admin`, replace the relevant wildcard with: `r.With(bearer.RequireRole("operator")).Post("/keys/rotate-signing", h.RotateSigning)`; `r.Post("/signed-urls/revoke", h.RevokeSignedURL)`; `r.Post("/signed-urls/sign", h.SignSignedURL)` (the latter two inherit the group's operator+moderator guard). Keep `adminNotFound` for the rest.

- [ ] **Step 6: Build + unit suite.** `go build ./... && go test ./internal/... ./pkg/... ./nova-image/... -short`. Expected PASS.

- [ ] **Step 7: gofmt; commit** (`feat(coordinator): wire signed-url Guard onto /blob + /i/*, admin routes, config knobs`).

---

## Task 12: integration test — end-to-end through nginx

**Files:**
- Create: `internal/integration/m7_signed_urls_test.go`

- [ ] **Step 1: Implement** the scenario in the design § "Integration" (steps 1–8): private-blob 401 baseline; mint → curl 200; tamper/aud/expiry → 403; revoke('cid') → 403; rotate + grace + shred → old-kid 403, new-kid 200; authz matrix (moderator can revoke, cannot rotate); public passthrough 200. Reuse the M6 nginx + Postgres testcontainer harness and the operator-login helper.

- [ ] **Step 2: Run** `go test ./internal/integration/ -run M7 -v`. Expected PASS (Docker required; `-short` skips).

- [ ] **Step 3: Commit** (`test(m7): end-to-end signed-url verify/rotate/revoke through nginx`).

---

## Task 13: documentation reconciliations

**Files:**
- Modify: `docs/specs/DATA_MODEL.sql`, `docs/specs/SIGNED_URL_FORMAT.md`, `docs/specs/openapi.yaml`, `docs/ROADMAP.md`, `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md`

- [ ] **Step 1: DATA_MODEL.sql** — fix the `key_state` enum comment: `'retired'` = rotated out, still verifies until `retire_after`; `'shredded'` = past grace, `wrapped_key` zeroed. (Design reconciliation #1.)

- [ ] **Step 2: SIGNED_URL_FORMAT.md** — replace the placeholder vector row with the real vectors from `internal/auth/signedurl/testdata/vectors.txt`; change the § "Verification" M6.2 note from "lands in M7" to "implemented in M7 (`internal/auth/signedurl`)"; add a one-liner that minting is server-side via `POST /api/v1/admin/signed-urls/sign`.

- [ ] **Step 3: openapi.yaml** — add `POST /api/v1/admin/signed-urls/sign` (`SignRequest{path, ttl_seconds, aud}` → `SignedURLResponse{url, kid, exp}`); reconcile the existing `rotate-signing` (`{grace_seconds?}` → `{kid, grace_expires_at}`) and `signed-urls/revoke` (`{kind, value}` → `{kind, value, revoked_at}`) schemas with the handlers; document the `403 invalid_signature` read response.

- [ ] **Step 4: ROADMAP + master plan** — mark M7 status; add the mint endpoint to the M7 line; link this design + plan.

- [ ] **Step 5: Validate + commit.**

```bash
npx --yes @redocly/cli lint docs/specs/openapi.yaml 2>&1 | tail -5 || echo "(no redocly; skipping)"
git add docs/specs/DATA_MODEL.sql docs/specs/SIGNED_URL_FORMAT.md docs/specs/openapi.yaml docs/ROADMAP.md docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md
git commit -m "docs(m7): fill signed-url vectors; openapi sign endpoint; key_state comment; roadmap"
```

---

## Task 14: full suite + finish the branch

- [ ] **Step 1: Full unit + short suite.** `go build ./... && go test ./... -short`. Expected PASS. (Per the gofmt-skew note, `gofmt -l` may flag pre-existing files; ensure only files you touched are formatted.)

- [ ] **Step 2: M7 integration.** `go test ./internal/integration/ -run M7 -v`. Expected PASS.

- [ ] **Step 3: Untagged build is the verified path.** `go build -o /tmp/nova-coordinator ./cmd/coordinator && echo OK`.

- [ ] **Step 4: Finish the branch.** Use `superpowers:finishing-a-development-branch` → fast-forward merge `m7-signed-urls` into `main` + annotated tag `m7-signed-urls` (local; no remote push), per the milestone workflow.

---

## Notes for the implementer

- **TDD discipline:** every package task is failing-test → run-FAIL → implement → run-PASS → commit. Do not batch.
- **Conform to `SIGNED_URL_FORMAT.md` exactly:** canonical string has **no trailing newline**; `sig` is base64url **without padding**; clock-skew tolerance is **0 s** (`exp == now` is expired). The spec is normative — drift is a bug.
- **No timing oracle:** the six `Verify` failure modes must be time-indistinguishable. Always run the constant-time HMAC compare (against a dummy key on earlier-step failures) so the response time does not reveal which check failed.
- **Never log secrets:** not the unwrapped key, not the `sig`, not the full signed URL. Log the *event* + parsed kid/aud/cid + the `signature_*` reason.
- **Generated-type drift:** read `internal/db/gen/models.go` after Task 0 — `retire_after` is `pgtype.Timestamptz`, `master_key_version_id` is the keystore's UUID type, `state` may be a generated enum or `string`; adapt the rotation/shred/bootstrap call sites accordingly.
- **Wrapped-key size:** a 256-bit secret wraps to **72 bytes** (24 nonce + 32 ct + 16 tag); the shred zero-buffer is `make([]byte, 72)`, matching the DEK convention in `DATA_MODEL.sql`.
- **Dependency direction:** the read-grant ctx key lives in `pkg/coordinator/storage`; `signedurl` (and the Guard) import `storage`, never the reverse — keep it acyclic.
- **Single-node pubsub:** revocation/key invalidation is in-process (`Invalidate`) + periodic refresh; the cross-node fan-out is Phase 2 — do not build it here.
- **Test isolation:** seed `master_key_versions` before `signing_keys` (FK); the KeySource/rotation tests need a bootstrapped keystore (reuse the M2/M6 keystore test helper).
