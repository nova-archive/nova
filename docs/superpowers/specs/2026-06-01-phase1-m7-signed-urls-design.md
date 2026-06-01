# Phase 1 M7 — Signed URLs + Signing-Key Rotation Design

## Purpose and scope

M7 is the seventh Phase 1 milestone and the second of the backend-capability band (M6–M10).
It makes Nova's private read paths actually reachable. Since M3 the read surface
(`/blob/{cid}`, `/i/*`) has gated private content — `storage.Resolve` returns
`ErrBlobAuthRequired` for any blob with no public/unlisted collection membership
(`pkg/coordinator/storage/blob.go:113`), which the handlers map to `401 signed_url_required`
(`internal/api/handlers/blob.go:44`). But there has been **no way to satisfy that gate**: the
read routes are mounted public (outside the bearer group), so a private blob is currently
un-retrievable by anyone. M7 supplies the missing credential — a **signed-URL HMAC verifier**
that hands a viewer a time-limited, audience-bound link to a private blob without requiring an
account, plus the signing-key rotation and revocation machinery that keep those links
operationally safe.

The wire format is already **normative**: `docs/specs/SIGNED_URL_FORMAT.md` (Phase 0 v2)
specifies the canonical string, the six-step verification order, the failure codes, the
rotation grace window, and the structured `(kind, value)` revocation scheme. M7 *implements*
that spec exactly — drift between the two is a bug in the implementation. The database objects
already exist: `signing_keys`, `signed_url_revocations`, and the `key_state` enum all ship in
`0001_init.sql`. **M7 adds no new tables and no migration.** The keystore
(`internal/envelope/keystore.go`) already wraps/unwraps secrets under the operator master key.

### In scope

- **`internal/auth/signedurl`**: the verifier package — canonical-string builder,
  HMAC-SHA256 `Sign`, base64url (no padding), the six-step `Verify`, a cached `KeySource`
  (DB + `keystore.Unwrap`), a cached `Revocations` set, a `Guard` chi middleware, and `Mint`.
- **`internal/auth/signedurl/testdata/vectors.txt`**: the authoritative test-vector fixture
  the format spec forward-references; back-fills the placeholder vector table in
  `SIGNED_URL_FORMAT.md`.
- **Signing-key rotation**: `POST /api/v1/admin/keys/rotate-signing` (operator) — mint a new
  active key, retire the prior active with `retire_after = now() + grace_window`.
- **Structured revocation**: `POST /api/v1/admin/signed-urls/revoke` (operator+moderator) —
  write a `(kind, value)` row; `kind ∈ {cid, aud, kid, path_prefix}`; immediate effect.
- **Minting**: `POST /api/v1/admin/signed-urls/sign` (operator+moderator) — `{path,
  ttl_seconds, aud}` → a ready signed URL. Minting needs the **unwrapped** HMAC secret, which
  only the coordinator holds (master key + DB), so it is necessarily a server-side operation.
- **Read-path integration**: `Guard` mounted on `/blob/{cid}` and `/i/*`; on a verified sig it
  sets a request-scoped read grant that lets `Resolve` serve the otherwise-private blob.
- **Key lifecycle**: auto-bootstrap of the first active signing key on first boot; a
  retired→shredded sweep (zeroes `wrapped_key`) in the coordinator GC loop; a `signing_keys`
  readiness probe on `/readyz`.
- **`novactl signed-url sign`**: CLI wrapper over the mint endpoint (the operator's hand-out
  tool), extending the M6 `novactl` binary.
- Unit + nginx-fronted integration tests; CI exercises the untagged binary.

### Out of scope (with the milestone that owns each)

- **Cross-node revocation propagation (pubsub).** The format spec mentions an "internal
  pubsub message" so a multi-node cluster invalidates immediately. Phase 1 is single-node:
  M7 uses **in-process invalidation** (the revoke handler pokes the in-memory set) plus the
  periodic refresh. The pubsub fan-out lands with **federation (Phase 2)**.
- **Master-key rotation tooling** — **M10**. Signing keys are a distinct key class from the
  master key; M7 rotates the HMAC signing keys, not the master key that wraps them.
  **Forward dependency:** `signing_keys.wrapped_key` is the first non-DEK table wrapped under
  the master key, so M10's re-wrap worker MUST also walk `signing_keys` (state `active` +
  within-grace `retired`) or it orphans every signing key on rotation — see § "Risks and
  notes" and reconciliation #6.
- **Admin SPA surfacing key rotation / URL minting** — **M11**. M7 is backend + `novactl`.
- **`audit_log` DB writer for rotate/revoke/sign actions** — **M9** (lands with the first
  takedown flows). M7 emits **structured logs** for these operator actions, consistent with
  M6's posture (the table exists; the writer is deferred).
- **Per-viewer binding / URL encryption** — explicitly non-goals of the format
  (`SIGNED_URL_FORMAT.md` § "What the format does not do"); not revisited here.

## Source of truth and required doc reconciliations

1. **`docs/specs/DATA_MODEL.sql` — fix the `key_state` enum comment (semantic drift).** The
   enum comment reads `'retired'  -- signing keys past their grace window`, but the normative
   `SIGNED_URL_FORMAT.md` § "Key rotation" uses `retired` for a key that is **rotated out but
   still verifiable until `retire_after`**, and `shredded` for the past-grace, key-zeroed
   terminal state. Correct the comment to: `'retired'` = rotated out, still verifies until
   `retire_after`; `'shredded'` = past grace, `wrapped_key` zeroed. (Schema objects are
   migration-only per the M6 reconciliation; this is a comment fix to the Phase-0 baseline.)

2. **`docs/specs/SIGNED_URL_FORMAT.md` — fill the test vectors; flip the status note.**
   Replace the placeholder vector row (§ "Test vectors", the "(placeholder — to be filled…)"
   line) with the real vectors generated into `internal/auth/signedurl/testdata/vectors.txt`.
   Update the M6.2 § "Verification" note from "the verifier code path … lands in M7" to
   "implemented in M7 (`internal/auth/signedurl`)", and add a one-line note that minting is
   server-side via `POST /api/v1/admin/signed-urls/sign`.

3. **`docs/specs/openapi.yaml` — add the mint path; confirm the two existing ones.** The spec
   already defines `POST /api/v1/admin/keys/rotate-signing` and
   `POST /api/v1/admin/signed-urls/revoke`. M7 **adds** `POST /api/v1/admin/signed-urls/sign`
   (`SignRequest` → `SignedURLResponse`), and reconciles the existing two entries' request/
   response schemas with the implemented handlers. Document the `403 invalid_signature` read
   error and its `code` values.

4. **`docs/ROADMAP.md` + `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md` — M7
   row.** Mark M7 status and link this design + its plan. Amend the M7 line to include the
   mint endpoint (the roadmap currently lists verifier + rotation + revocation only). Record
   the `m7-signed-urls` tag on completion.

5. **`internal/config/types.go`** — add a `SignedURLs` section (fields + comments only).

6. **Master-key re-wrap of signing keys (raised in review) —
   `docs/specs/ENCRYPTION_ENVELOPE.md`, master plan M10, `DATA_MODEL.sql`.** M7 introduces
   the first non-DEK table (`signing_keys`) holding a `wrapped_key` encrypted under the master
   key, so master-key rotation (M10) must re-wrap it too. The envelope spec already says "data
   **and** signing" (§ "Master-key versioning") and its rotation procedure has a "Same for
   `signing_keys`" step, but that step inherited the DEK `state='active'` filter — which would
   silently orphan **within-grace `retired`** signing keys that are still verifying. M7
   tightens ENCRYPTION_ENVELOPE.md step 4 to `state IN ('active','retired')`, adds the
   signing-key re-wrap to the M10 deliverables/exit, and notes the obligation in the
   `signing_keys` schema comment. (No M7 code change — this is a documented M10 dependency.)

---

## Preconditions from M1–M6 (confirmed in committed code)

- **Schema present** (`internal/db/migrations/0001_init.sql`):
  - `signing_keys (kid PK text, algorithm, wrapped_key bytea, master_key_version_id uuid FK,
    state key_state DEFAULT 'active', active_from, retire_after, created_at)`;
    `signing_keys_state_idx` on `state`.
  - `signed_url_revocations (id, kind text CHECK kind IN ('cid','aud','kid','path_prefix'),
    value text, revoked_at, UNIQUE(kind,value))`; `signed_url_revocations_kind_value_idx`.
  - `key_state` enum = `active | rotating | shredded | retired`.
- **Keystore** (`internal/envelope/keystore.go`): `Wrap(secret) ([]byte, uuid.UUID, error)`
  returns the 72-byte wrapped payload + the active `master_key_versions.id`;
  `Unwrap(ctx, wrapped, versionID) ([]byte, error)` recovers it. A 256-bit secret wraps to 72
  bytes (24-byte nonce + 32 ct + 16 tag), matching the DEK convention.
- **Read path** gates private content in `Resolve` (`storage/blob.go:108–115`):
  `ResolveEffectiveVisibility` → `resolveVisibility` → `ErrBlobAuthRequired` when private.
  `BlobView.Visibility` already drives `Cache-Control` (`private, max-age=300` vs
  `public, …, immutable`). Both `/blob` (`internal/api/handlers/blob.go`) and `/i/*`
  (`nova-image/internal/imageapi/handler.go`, via `resolveImageParent`) funnel through it.
- **Router** (`internal/api/server.go`): `/blob/{cid}` GET/HEAD mounted public at the root
  (lines 73–76). The `/api/v1/admin` group is guarded `RequireRole("operator","moderator")`
  and currently routes everything to `adminNotFound` — whose comment reads "M7–M10 will add
  them" (`server.go:174`). `/i/*` is mounted by the image product via
  `RegisterProduct → p.RegisterRoutes(c.mux)` on the root mux (`coordinator.go:265`).
- **GC loop** (`coordinator.go:356`): a periodic ticker already sweeps upload sessions,
  expired/revoked refresh tokens, and rate-limiter buckets — the natural home for the shred
  sweep.
- **/readyz** (`internal/api/handlers/ready.go`): `ReadyCheck{Name, Fn func(ctx) error}`
  registered in `coordinator.New` (DB, IPFS, verifier readiness). M6.2 D1.
- **`novactl`** (`cmd/novactl/main.go`): the M6 binary; `usage()` and an `os.Args` switch
  dispatch `auth login|whoami|logout`. Reads/writes `~/.config/nova/credentials.json` (0600).
- **`cmd/coordinator/main.go`** is env-driven; builds the keystore with
  `envelope.NewKeystoreFromEnv(pool)` then `ks.Bootstrap(ctx)`; resolves secrets through
  `config.ResolveSecret(env, _FILE, /run/secrets/…)`.

---

## Architecture

```
   viewer ── GET /blob/{cid}?sig&exp&aud&kid   (Origin / Referer header)
             GET /i/{cid}/p/thumb.webp?sig…
                          │
        request-id, recover, ratelimit (global)
                          │
                 signedurl.Guard  ──── no sig params ────► next handler
                          │                                (anonymous; private ⇒ 401)
                          │ sig params present
                          ▼
        Verify (constant-time, 0 s skew):
          1 schema  2 kid  3 revocation  4 expiry  5 HMAC  6 audience
              │ any fail                         │ all pass
              ▼                                  ▼
   403 + signature_* code              ctx = storage.WithReadAuthz(ctx)
                                                 │
                                       handler ─► Resolve  ── private gate
                                                 │            bypassed by grant
                                                 ▼
                                         200  (decrypt + serve)

   admin (bearer): POST /api/v1/admin/keys/rotate-signing      (operator)
                   POST /api/v1/admin/signed-urls/revoke        (operator+moderator)
                   POST /api/v1/admin/signed-urls/sign          (operator+moderator)

   KeySource  : signing_keys + keystore.Unwrap, short-TTL cache, Invalidate()
   Revocations: signed_url_revocations, load at boot + refresh 30 s + Invalidate()
   gcLoop     : retired & retire_after≤now ⇒ state='shredded', wrapped_key=zeros(72)
```

The **`Guard` is fail-through, not fail-closed.** A request with *no* signed-URL parameters
passes straight to the content handler unchanged — public blobs still serve anonymously, and
a private blob still 401s. The Guard only engages when at least one of `sig|exp|aud|kid` is
present, which marks the request as *claiming* signed-URL authorization; from that point a
malformed or invalid claim is a hard `403 invalid_signature`. This keeps the signed-URL path
strictly additive to the M3 read semantics.

### Package boundaries

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/auth/signedurl` | canonical string, Sign/Verify, KeySource, Revocations, Guard, Mint | envelope (Unwrap), db/gen, storage (read-grant ctx), crypto/hmac+subtle |
| `pkg/coordinator/storage` | adds `WithReadAuthz`/`readAuthorized` ctx helpers; `Resolve` consults the grant | — (ctx only) |
| `internal/api/handlers` | the three admin handlers (`signing_admin.go`) | signedurl, bearer, httputil |
| `internal/api` (`server.go`) | mount admin routes; wrap `/blob` with Guard | signedurl |
| `pkg/coordinator` | build KeySource+Revocations+Guard; wrap `/i/*`; bootstrap; shred; readyz | signedurl, storage, envelope |
| `cmd/novactl` | `signed-url sign` subcommand | — (HTTP) |

The `signedurl → storage` dependency (for the read-grant ctx key) is acyclic: `storage` does
not import `signedurl`. The grant key lives in `storage` because `storage.Resolve` is its only
consumer; the Guard, constructed in the coordinator with its deps, sets it.

---

## Signed-URL format (normative recap)

Full normative text: `docs/specs/SIGNED_URL_FORMAT.md`. The implementation conforms exactly.

**Wire**: `{path}?sig={b64url}&exp={unix_s}&aud={origin}&kid={key_id}` — all four required,
any order. Missing any ⇒ `403 invalid_signature` `signature_missing_param`.

**Canonical string** (fed to HMAC-SHA256), LF-separated, **no trailing newline**:

```
canonical = path + "\n" + exp + "\n" + aud + "\n" + kid
```

- `path` = `r.URL.Path` (already percent-decoded; leading slash; no query).
- `exp` = base-10 integer, no leading zeros.
- `aud` = the decoded query value (an origin, e.g. `https://example.com`).
- `kid` = the literal key id.

`sig = base64url(HMAC-SHA256(unwrapped_key, canonical))`, **no padding**.

```go
// internal/auth/signedurl/signedurl.go
func Canonical(path string, exp int64, aud, kid string) string {
    return path + "\n" + strconv.FormatInt(exp, 10) + "\n" + aud + "\n" + kid
}

func Sign(key []byte, canonical string) string {
    m := hmac.New(sha256.New, key)
    m.Write([]byte(canonical))
    return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
```

## Verification flow

`Verify` runs the six steps in order; **any** failure short-circuits to `403
invalid_signature` with a specific `code`; comparisons are constant-time and the error path is
uniform in time (no oracle distinguishing failure modes). Clock-skew tolerance is **0 s**
(operators must run NTP).

| # | Step | Source of truth | Failure `code` |
|---|---|---|---|
| 1 | **Schema** — all four params present & well-formed (`exp` parses, `sig` decodes) | query | `signature_missing_param` |
| 2 | **Key lookup** — `signing_keys` row for `kid`, `state='active'` OR (`state='retired'` AND `retire_after > now()`) | `KeySource.ByKID` | `signature_unknown_kid` |
| 3 | **Revocation** — none of `('cid',{cid})`, `('aud',{aud})`, `('kid',{kid})`, `('path_prefix', p)` with `p` a prefix of `{path}` | `Revocations` | `signature_revoked` |
| 4 | **Expiry** — `exp > now_unix()` | clock | `signature_expired` |
| 5 | **Signature** — `subtle.ConstantTimeCompare(HMAC(unwrap(key), canonical), sig) == 1` | `KeySource` + `keystore.Unwrap` | `signature_invalid` |
| 6 | **Audience** — parse `Origin` (else `Referer`) → `scheme://host[:port]`, equal to `{aud}` byte-for-byte | request headers | `signature_aud_mismatch` |

The `{cid}` in step 3 is parsed from `{path}`: the leading content segment of `/blob/{cid}`
or `/i/{cid}/…`. The verifier exposes a `Decision` (allow + the canonical fields, or
deny + code) so the Guard can both authorize and emit a precise structured log line.

```go
type VerifyInput struct {
    Path  string            // r.URL.Path
    Query url.Values        // sig, exp, aud, kid
    Origin, Referer string  // headers for step 6
    Now   time.Time
}
type Decision struct {
    OK   bool
    Code string            // failure code when !OK
    CID, Aud, Kid string   // parsed fields (logging)
}
func (v *Verifier) Verify(ctx context.Context, in VerifyInput) Decision
```

## Signing-key lifecycle

States use the existing `key_state` enum. A signed URL verifies against a key that is
`active`, or `retired` and still inside its grace window.

- **kid** is an opaque, unique identifier (`signing_keys.kid` is the PK). M7 generates it as
  `"k_" + base32(crypto/rand 10 bytes)` (lower-case, no padding). The `k_2026_05` in the
  format spec is illustrative only; nothing parses meaning out of a kid.
- **Bootstrap (first boot).** At startup, after `ks.Bootstrap`, if
  `COUNT(*) WHERE state='active' = 0`, generate a 256-bit secret, `ks.Wrap` it, and insert an
  `active` row. Idempotent and safe to run every boot. This is what makes signed URLs work out
  of the box; without it, verification would return `signature_unknown_kid` until a manual
  rotate.
- **Rotate** (`rotate-signing`). In one transaction:
  1. generate a 256-bit secret → `ks.Wrap` (binds the current `master_key_version_id`);
  2. `InsertSigningKey(new_kid, 'active', active_from=now)`;
  3. `RetirePriorActiveSigningKey`: set the previously-active key `state='retired',
     retire_after = now() + grace`.
  Then invalidate the key cache. URLs minted under the prior key keep verifying until
  `retire_after` (the verifier looks keys up *by kid*, never "the current default").
  Returns `201 {kid, grace_expires_at}`.
- **Shred** (GC sweep). `ShredExpiredRetiredSigningKeys`:
  `UPDATE signing_keys SET state='shredded', wrapped_key = <72 zero bytes>
   WHERE state='retired' AND retire_after <= now()`. Mirrors the DEK crypto-shred and runs in
  the existing `gcLoop`. After shred, the kid resolves but is past-grace ⇒
  `signature_unknown_kid` (step 2), and the key bytes are gone.

An operator who suspects a key leak revokes `('kid', kid)` (immediate, step 3) **and** rotates
— revocation stops verification instantly while shred waits for grace.

## Revocation

`POST /api/v1/admin/signed-urls/revoke` writes one `(kind, value)` row (idempotent via
`ON CONFLICT (kind, value) DO NOTHING`) and invalidates the in-memory set. `kind` is validated
against the same four values the DB `CHECK` enforces. Effects (verifier step 3):

| `kind` | `value` | Effect |
|---|---|---|
| `cid` | a CID | every signed URL for that CID fails |
| `aud` | an origin | every URL bound to that origin fails |
| `kid` | a signing key id | every URL signed with that key fails (instant, ahead of shred) |
| `path_prefix` | a path prefix (e.g. `/i/bafy.../`) | every URL whose path starts with it fails |

`Revocations` loads the table at construction, refreshes every
`revocation_refresh_seconds` (default 30), and exposes `Invalidate()` for the immediate,
in-process poke from the revoke handler. `IsRevoked(cid, aud, kid, path)` is a map/slice
lookup; `path_prefix` is the one linear scan (prefixes are few).

## Minting

`POST /api/v1/admin/signed-urls/sign` (operator+moderator) takes `{path, ttl_seconds, aud}`
and returns `{url, kid, exp}`:

1. validate `path` is a readable content path (`/blob/{cid}` or `/i/{cid}/…`) and `aud` is a
   well-formed origin;
2. clamp `ttl_seconds` to `[1, max_ttl_seconds]` (default cap 86400);
3. `exp = now + ttl`; fetch the **active** key (`KeySource.Active`, unwrapped);
4. `sig = Sign(key, Canonical(path, exp, aud, kid))`;
5. return the assembled URL `path?sig&exp&aud&kid`.

Minting is operator/moderator-gated because it confers read access to private content. The
`novactl signed-url sign --path … --ttl … --aud …` subcommand POSTs this endpoint with the
cached bearer token and prints the URL — the operator's hand-out tool. (The endpoint, not the
CLI, is the trust boundary; `novactl` is a thin client.)

## Read-path integration

A successful `Verify` authorizes **this request** to read private content. The Guard records
that as a request-scoped grant in the context; `Resolve` consults it:

```go
// pkg/coordinator/storage/grant.go
type readAuthzKey struct{}
func WithReadAuthz(ctx context.Context) context.Context {
    return context.WithValue(ctx, readAuthzKey{}, true)
}
func readAuthorized(ctx context.Context) bool {
    v, _ := ctx.Value(readAuthzKey{}).(bool)
    return v
}

// storage/blob.go — the only change to Resolve:
if visibility == VisibilityPrivate && !readAuthorized(ctx) {
    return nil, ErrBlobAuthRequired
}
```

This is sound because the HMAC binds the signature to the exact `r.URL.Path`: a sig minted for
`/blob/X` cannot validate against a request for `/blob/Y`, so the grant can never leak across
content. The grant is request-scoped (not stored, not cross-request) and covers the parent CID
plus any derivative reached *through* the authorized path (e.g. the `/i/{cid}/p/thumb.webp`
derivative resolved inside `transformAndServe`). `Visibility` on the `BlobView` is unchanged,
so a signed-URL-served private blob still gets `Cache-Control: private, max-age=300` — correct,
since the URL is time-limited and audience-bound.

**Wiring the Guard onto both read trees.** The Guard is one `func(http.Handler) http.Handler`,
constructed in `coordinator.New` when `pool`/`ks` are present:

- `/blob/{cid}` (+ `.json`, HEAD): passed into `api.ServerConfig` and applied with
  `r.With(guard)` in `server.go`.
- `/i/*`: `RegisterProduct` mounts product routes under a group carrying the Guard
  (`c.mux.Group(func(r){ r.Use(guard); p.RegisterRoutes(r) })`) instead of mounting directly
  on `c.mux`. The reserved-prefix probe is unchanged. When no Guard is built (no pool/ks test
  coordinator), routes mount as today.

## Configuration additions (`internal/config`)

```go
// internal/config/types.go
type SignedURLs struct {
    GraceWindowSeconds       int `yaml:"grace_window_seconds"`        // rotation grace; default 86400
    RevocationRefreshSeconds int `yaml:"revocation_refresh_seconds"`  // cache refresh; default 30
    KeyCacheTTLSeconds       int `yaml:"key_cache_ttl_seconds"`       // unwrapped-key cache; default 60
    MaxTTLSeconds            int `yaml:"max_ttl_seconds"`             // mint ttl cap; default 86400
}
```

Following the M5/M6 precedent (operator.yaml is not yet wired into `cmd`), the coordinator
reads env knobs and maps them onto `coordinator.Config`:
`NOVA_SIGNED_URL_GRACE_SECONDS`, `NOVA_SIGNED_URL_REVOCATION_REFRESH_SECONDS`,
`NOVA_SIGNED_URL_KEY_CACHE_TTL_SECONDS`, `NOVA_SIGNED_URL_MAX_TTL_SECONDS`. Each falls back to
the default above when unset/zero.

## Startup validation changes

- **Auto-bootstrap** the first active signing key (above) — runs after `ks.Bootstrap`, before
  serving, so a freshly-initialised node can verify signed URLs immediately.
- No refuse-to-start is added: a signing key being *absent* is auto-healed, not fatal. A
  `master_key` that cannot wrap (the existing keystore precondition) already fails the boot.

## HTTP contract

### Routes added / implemented

| Method | Path | Auth | Notes |
|---|---|---|---|
| POST | `/api/v1/admin/keys/rotate-signing` | RequireRole(operator) | `{grace_seconds?}` → 201 `{kid, grace_expires_at}` |
| POST | `/api/v1/admin/signed-urls/revoke` | RequireRole(operator,moderator) | `{kind, value}` → 201 `{kind, value, revoked_at}`; idempotent |
| POST | `/api/v1/admin/signed-urls/sign` | RequireRole(operator,moderator) | `{path, ttl_seconds, aud}` → 201 `{url, kid, exp}` |
| (mw) | `/blob/{cid}`, `/blob/{cid}.json`, `/i/*` | signed URL or anonymous | Guard: 403 on bad sig; pass-through when no sig params |

`rotate-signing` tightens the inherited `/admin` guard to operator-only with an inner
`RequireRole("operator")`. The first two paths replace the `adminNotFound` wildcard for their
routes; unknown `/admin/*` still 404s after the guard.

### Error → status

| Condition | Status | `code` |
|---|---|---|
| signed-URL request, any of the six checks fails | 403 | one of `signature_missing_param`, `signature_unknown_kid`, `signature_revoked`, `signature_expired`, `signature_invalid`, `signature_aud_mismatch` (per § "Verification flow") |
| private blob, **no** signed-URL params, no bearer | 401 | `signed_url_required` (unchanged, M3) |
| rotate/revoke/sign without a token | 401 | `unauthenticated` (bearer guard) |
| rotate by a non-operator (e.g. moderator) | 403 | `forbidden` |
| revoke with `kind ∉ {cid,aud,kid,path_prefix}` | 400 | `invalid_kind` |
| sign with a non-content `path` or malformed `aud` | 400 | `invalid_request` |

The body `code` carries the **specific** `signature_*` reason — `SIGNED_URL_FORMAT.md` is
normative on these and assigns one per check ("Failure code: …"). There is no generic
`invalid_signature` code value; "the invalid_signature error" is the prose name for the 403
class. Response **timing** is uniform across all six failures (constant-time HMAC compare +
equalised error path), so no timing oracle leaks even though the returned code names the
failed check — a deliberate spec trade-off (precise codes aid operators and clients; timing
equalisation defeats timing attacks).

---

## Testing strategy

### Unit

- **canonical / sign**: exact canonical bytes for the spec's worked example; base64url has no
  `=`; round-trip `Sign`→verify with a known key reproduces the committed test vectors
  (`testdata/vectors.txt`).
- **Verify, per code**: a table driving each failure exactly once — missing each param;
  unknown kid; retired-past-grace kid; each revocation kind (incl. `path_prefix` hit/miss);
  expired (`exp == now` rejects, `now+1` passes — 0 s skew); tampered sig; `Origin` vs
  `Referer` fallback and a port-mismatch `aud`. Assert constant-time compare is used and the
  six failure timings are indistinguishable (coarse).
- **KeySource**: `ByKID` returns active and within-grace retired, rejects past-grace and
  shredded; `Active` returns the newest active; the TTL cache serves hits and `Invalidate`
  forces a reload; `Unwrap` failure surfaces (not panics).
- **Revocations**: load, refresh picks up an inserted row, `Invalidate` is immediate,
  `IsRevoked` matches each kind.
- **rotate**: new active inserted; prior active → retired with `retire_after ≈ now+grace`;
  cache invalidated; concurrent rotate is serialised (one active at a time).
- **shred**: retired-past-grace → shredded with a 72-byte zero `wrapped_key`; within-grace
  untouched.
- **bootstrap**: empty table → one active key; second call is a no-op.
- **mint**: ttl clamped to the cap; non-content path rejected; assembled URL re-verifies.
- **storage grant**: `Resolve` returns a private `BlobView` under `WithReadAuthz`, and
  `ErrBlobAuthRequired` without it; public unaffected either way.

### Integration (`internal/integration/m7_signed_urls_test.go`, nginx-fronted, testcontainers)

End-to-end against the untagged coordinator behind nginx (Postgres + nginx containers):

1. Boot, keystore + auto-bootstrapped signing key; create an `operator`; upload a blob into a
   **private** collection.
2. Baseline: `GET /blob/{cid}` with no sig → **401 `signed_url_required`**.
3. `POST /api/v1/admin/signed-urls/sign` (operator) for that path, ttl 300, `aud =
   https://embed.example` → a URL; `curl` it with a matching `Origin` → **200**, bytes match.
4. Tamper one `sig` char → **403** (`signature_invalid`). Wrong `Origin` → **403**
   (`signature_aud_mismatch`). `exp` in the past → **403** (`signature_expired`).
5. `POST …/signed-urls/revoke` `('cid', {cid})` → the same URL now **403** (`signature_revoked`).
6. Rotation: `POST …/keys/rotate-signing` (grace 2 s); a URL minted under the **old** kid still
   verifies (within grace); run the shred sweep after grace → old-kid URL **403**
   (`signature_unknown_kid`); a freshly-minted URL (new active kid) → **200**.
7. Authz matrix: `rotate-signing` with a moderator token → **403 forbidden**; with no token →
   **401**; `revoke` with a moderator token → **allowed**.
8. Passthrough: a **public** blob with no sig → **200** (Guard inert), confirming M3 read
   semantics are intact.

### CI

- Build + unit-test the untagged binary (the production path).
- Integration is `-short`-skippable like M2–M6; full run in the integration job.
- sqlc codegen-check after regenerating from `signedurl.sql`.

---

## Security and privacy considerations

- **Constant-time everywhere**: `subtle.ConstantTimeCompare` on the HMAC; the six failure
  paths are time-equalised so no failure-mode oracle leaks (matches the format spec).
- **0 s clock skew**: strict expiry; operators run NTP (documented). No grace on `exp`.
- **Secrets**: HMAC signing keys are stored only `wrapped` (under the master key) and are
  unwrapped transiently for sign/verify; the raw key, the master key, and full signed URLs are
  **never logged**. Structured logs record the *event* (mint/rotate/revoke, and verify
  failures with their `signature_*` reason + parsed kid/aud/cid) — never the secret or the sig.
- **Least privilege**: minting and rotation are admin-gated; rotation is operator-only;
  minting/revocation include moderators (takedown + share workflows). The Guard grants a
  **request-scoped, path-bound** read only — it cannot escalate to writes (writes require a
  bearer token; signed URLs are GET-only) and cannot reach a different CID.
- **Revocation is immediate and (within a node) cluster-wide**; the leaked-URL replay window
  is the operator's chosen TTL. Guidance (per spec): short TTLs + revoke for incident response.
- **Crypto-shred**: past-grace keys have their `wrapped_key` zeroed; autovacuum reclaims the
  bytes, so a later DB dump yields no retired key material.
- **Paranoid mode** unaffected: no new IP retention; signed-URL `aud` is an origin, not a
  viewer identifier; verify failures log the origin (already in the request), not the viewer.

---

## Risks and notes

- **Master-key rotation must re-wrap signing keys (forward dependency on M10).**
  `signing_keys.wrapped_key` is wrapped under the operator master key (via
  `master_key_version_id`), exactly like `data_encryption_keys`. When M10 implements
  `rotate-master`, its re-wrap transaction MUST process `signing_keys` rows in state `active`
  **and** within-grace `retired` (both still verify signed URLs) alongside the blob DEKs;
  missing them orphans every signing key on rotation and breaks signed-URL verification
  cluster-wide. The obligation is now captured in three co-located places so it cannot be
  dropped: `ENCRYPTION_ENVELOPE.md` § "Rotation procedure" step 4 (tightened to the
  `active`+`retired` filter), the M10 deliverables/exit in the master plan, and the
  `signing_keys` schema comment (reconciliation #6). M7 itself ships no rotate-master code.
- **Single-node revocation only.** In-process `Invalidate` + 30 s refresh is immediate on the
  one node; a future multi-node cluster needs the pubsub fan-out (Phase 2). Documented as the
  bound; the periodic refresh is the backstop if an invalidate is ever missed.
- **Grant is a boolean, not a CID-scoped capability.** Accepted: the HMAC already binds the
  authorization to the exact path, so a per-request boolean cannot over-authorize. A CID-scoped
  grant would add complexity (and break the derivative-resolution path) for no security gain.
- **Mint endpoint is new surface beyond the roadmap line.** Justified: without a server-side
  signer the feature is unusable before M11, and minting *requires* the unwrapped key that only
  the coordinator holds. Kept minimal (one handler + a `novactl` wrapper) and admin-gated.
- **`retire_after` semantics differ from the stale enum comment.** Reconciliation #1 fixes the
  comment; the implementation follows the normative format spec (retired = still-verifying
  within grace).
- **Key-unwrap on the read path.** Verify unwraps the signing key per request unless cached;
  the short-TTL key cache (default 60 s) bounds the `keystore.Unwrap` + DB cost while keeping
  rotation/shred latency-bounded. The cache holds only the *active* and within-grace keys.
- **`nova_dev` anonymous floor.** Under `-tags nova_dev` the read path may be anonymous; the
  Guard still verifies any *presented* sig (so dev exercises the real path), but private-gate
  bypass via `nova_dev` is unchanged and out of M7's scope.

---

## File structure

### Created in M7

```
internal/auth/signedurl/signedurl.go          Canonical, Sign, Verify (six-step), Decision
internal/auth/signedurl/signedurl_test.go
internal/auth/signedurl/keysource.go          KeySource: ByKID/Active, Unwrap, TTL cache, Invalidate
internal/auth/signedurl/keysource_test.go
internal/auth/signedurl/revocations.go        Revocations: load/refresh/Invalidate/IsRevoked
internal/auth/signedurl/revocations_test.go
internal/auth/signedurl/guard.go              chi middleware: detect → Verify → grant/403
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

### Modified in M7

```
internal/db/gen/*                             regenerated from signedurl.sql
pkg/coordinator/storage/blob.go               Resolve: private gate honors readAuthorized(ctx)
internal/api/server.go                        mount admin signed-URL routes; wrap /blob with Guard; operator-only rotate
pkg/coordinator/coordinator.go                build KeySource+Revocations+Guard; wrap /i/* in RegisterProduct; bootstrap; shred sweep; signing_keys readyz; ServerConfig fields
cmd/coordinator/main.go                       bootstrap first signing key; NOVA_SIGNED_URL_* env knobs
cmd/novactl/main.go                           signed-url sign subcommand + usage
internal/config/types.go                      SignedURLs section
docs/specs/DATA_MODEL.sql                     key_state comment reconciliation (#1)
docs/specs/SIGNED_URL_FORMAT.md               fill test vectors; flip M7 status note (#2)
docs/specs/openapi.yaml                       add /signed-urls/sign; reconcile rotate/revoke; invalid_signature codes (#3)
docs/ROADMAP.md                               M7 status + mint endpoint + tag (#4)
docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md   M7 row + link (#4)
```

### Reused unchanged

```
internal/envelope/keystore.go                 Wrap / Unwrap (signing-key wrap + recover)
internal/api/handlers/ready.go                ReadyCheck (signing_keys probe)
internal/auth/bearer/bearer.go                RequireRole guards
internal/api/httputil/                         WriteError (Error shape)
```

---

## Cross-references

- `docs/specs/SIGNED_URL_FORMAT.md` — the normative wire format, verification order, rotation,
  and revocation scheme M7 implements.
- `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md` § "M7 — Signed URLs +
  signing-key rotation".
- `docs/superpowers/plans/2026-06-01-phase1-m7-signed-urls.md` — the implementation plan.
- `docs/specs/DATA_MODEL.sql` — `signing_keys`, `signed_url_revocations`, `key_state`.
- `docs/specs/openapi.yaml` — `/api/v1/admin/keys/rotate-signing`, `/api/v1/admin/signed-urls/*`.
- `internal/envelope/keystore.go` — master-key wrap/unwrap (distinct key class from these
  HMAC signing keys; rotation of the *master* key is M10).
- `docs/THREAT_MODEL.md` — boundary ③ (secret resolver), private-content authorization.
- M6 design (`2026-05-30-phase1-m6-auth-design.md`) — the bearer guards M7's admin endpoints
  reuse, and the `novactl` binary M7 extends.
