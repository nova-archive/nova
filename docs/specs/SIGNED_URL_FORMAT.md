# Signed URL Format

Status: **Phase 0 v2 — normative.** Implementations of `internal/auth`
must conform exactly. Drift between this spec and the implementation
is a bug in the implementation.

> **v2 note.** The original revocation scheme (string-prefix match
> against the canonical signed-URL string) was incorrect: the
> canonical string starts with the URL path, so prefixes like
> `cid:{cid}` or `aud:{aud}` could never match. v2 replaces it with
> structured (kind, value) revocation entries against parsed fields.
> See § "Revocation".

## Purpose

Signed URLs let a Nova operator hand a viewer a time-limited,
audience-bound link to a blob without requiring the viewer to hold
credentials. They are used for:

- Private collections that are not exposed via anonymous read.
- Embeds that should expire after a session (chat attachments, etc.).
- Any rate-limited "share this for an hour" workflow.

Signed URLs are an alternative to bearer-token authentication. A
request with a valid `sig` is accepted regardless of whether a
bearer token is present.

## Wire format

A signed read URL is the canonical content URL plus four query
parameters appended in any order:

```
{path}?sig={signature}&exp={unix_seconds}&aud={origin}&kid={key_id}
```

| Param | Type | Description |
|---|---|---|
| `sig` | base64url-encoded bytes | HMAC-SHA256 of the canonical string. No padding. |
| `exp` | integer | Unix timestamp in seconds (UTC). After this moment the URL is rejected. |
| `aud` | string | Origin of the embedding context (e.g., `https://example.com`). |
| `kid` | string | Identifier of the signing key in the `keys` table. |

All four are required. Missing any parameter is a `403 invalid_signature` error.

### Example

```
GET /i/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi/p/thumb.webp?
    exp=1730000000&
    aud=https%3A%2F%2Fexample.com&
    kid=k_2026_05&
    sig=Wf8E3Ui1lP_ZhO3pJxV5T6m9DkB0v6oMZQyqr5QyXtA
```

## Canonical string

The string fed to HMAC-SHA256 is built deterministically as:

```
canonical = path + "\n" + exp + "\n" + aud + "\n" + kid
```

- `path` is the URL path **including a leading slash and excluding
  any query string.** It must be percent-decoded back to the canonical
  bytes the server will see (the server compares against its own
  `r.URL.Path` value, which is already decoded).
- `exp` is the integer rendered in base 10 with no leading zeros.
- `aud` is the literal string from the query parameter (not
  URL-encoded; the value the server reads after URL decoding).
- `kid` is the literal key identifier.
- The separator is a single ASCII LF byte (`0x0A`).

The string is hashed with HMAC-SHA256 using the bytes stored in the
`keys.wrapped_key` row referenced by `kid`, after unwrapping with
the operator master key.

The resulting 32 raw bytes are base64url-encoded **without padding**
to produce the `sig` parameter.

### Worked example

Given:

- `path = "/i/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi/p/thumb.webp"`
- `exp = 1730000000`
- `aud = "https://example.com"`
- `kid = "k_2026_05"`

The canonical string is (with `\n` rendered explicitly):

```
/i/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi/p/thumb.webp
1730000000
https://example.com
k_2026_05
```

That is exactly four lines, terminated by `\n` between fields and
**no trailing newline**.

## Verification

The server performs verification in this order. **Any failure short-
circuits with `403 invalid_signature` and an appropriate `code`
field in the JSON error body.** No timing differences between failure
modes are observable to the client (constant-time comparison
throughout, and uniform error response time).

1. **Schema check.** All four query parameters present, well-formed.
   Failure code: `signature_missing_param`.
2. **Key lookup.** `signing_keys` row where `kid = ?` exists and is
   either `state='active'` or `state='retired'` with
   `retire_after > now()`.
   Failure code: `signature_unknown_kid`.
3. **Revocation check.** Parse the canonical string into its fields
   (cid, aud, kid, path) and check each against
   `signed_url_revocations`:
   - any row with `kind='cid'` and `value={cid}` → revoked
   - any row with `kind='aud'` and `value={aud}` → revoked
   - any row with `kind='kid'` and `value={kid}` → revoked
   - any row with `kind='path_prefix'` and `value` is a prefix of `{path}` → revoked
   Failure code: `signature_revoked`.
4. **Expiry check.** `exp > now_unix_seconds()`. Servers must use a
   monotonic, NTP-synced clock; clock skew tolerance is **0 seconds**.
   Failure code: `signature_expired`.
5. **Signature recomputation.** Unwrap the `wrapped_key` for the
   referenced kid using the master key version recorded on the
   `signing_keys` row. Compute
   `expected = HMAC-SHA256(unwrapped_key, canonical)` and compare to
   the decoded `sig` bytes with a constant-time comparator
   (`crypto/subtle.ConstantTimeCompare`).
   Failure code: `signature_invalid`.
6. **Audience check.** The request's `Origin` header (or `Referer`
   if `Origin` is absent) is parsed for its scheme + host + port,
   and the resulting origin string must equal `aud` byte-for-byte.
   Failure code: `signature_aud_mismatch`.

If all six pass, the request is authorized and proceeds to the
content handler. The content handler must not re-check authorization;
the signed URL is sufficient.

## Key rotation

The operator rotates the signing key by calling
`POST /api/v1/admin/keys/rotate-signing`. The coordinator:

1. Generates a new 256-bit secret.
2. Inserts a new `signing_keys` row with a fresh `kid`,
   `state = 'active'`, `active_from = now()`,
   `master_key_version_id = <current active master version>`.
3. Marks the previous active signing key `state = 'retired'` and
   sets `retire_after = now() + grace_window` (default 24 h). URLs
   minted with the previous key remain verifiable until
   `retire_after`.
4. Returns the new `kid` so the caller can update any long-lived
   signed URLs they cache.

URLs minted between rotation and grace expiry are signed with the
**previous** key but verified against any non-retired-past-grace key
in the `signing_keys` table — the verifier looks up the key by
`kid`, not by "currently default."

After `retire_after` passes, a scheduled job transitions the
retired row's `state = 'shredded'` and zeroes the wrapped key.

## Revocation

v2 revocation is **structured**, not prefix-based. The original
prefix-against-canonical-string scheme could not represent the
documented `cid:` / `aud:` / `kid:` revocation forms because the
canonical string starts with the URL path.

`POST /api/v1/admin/signed-urls/revoke` writes a row into
`signed_url_revocations` with a `(kind, value)` tuple. The verifier
parses the canonical signed-URL string into its component fields
and checks each against the revocation table:

| `kind` | `value` | Effect |
|---|---|---|
| `cid` | a CID | Every signed URL for that CID fails verification |
| `aud` | an origin (e.g., `https://example.com`) | Every URL bound to that origin fails |
| `kid` | a signing key id | Every URL signed with that key fails (equivalent to instant key shred) |
| `path_prefix` | a URL path prefix (e.g., `/i/bafy.../`) | Every URL whose path starts with this prefix fails |

The schema is enforced by the `CHECK (kind IN (...))` constraint in
`DATA_MODEL.sql`.

Common operations:

- After a takedown: insert `('cid', {cid})`. The
  `crypto_shred(blob)` procedure does this automatically.
- After an embedding site compromise: insert `('aud', {origin})`.
- After a suspected signing-key leak: insert `('kid', {kid})` and
  trigger `POST /api/v1/admin/keys/rotate-signing`.

Revocations are immediate and cluster-wide; the verifier loads the
revocation table at startup and refreshes it every N seconds
(default 30) plus on demand via an internal pubsub message.

## What the format does **not** do

- It does not encrypt the URL. The CID, preset, exp, aud, and kid
  are all visible to anyone who can see the URL. If the operator
  also enables blob-level encryption (the default), the bytes
  served are still ciphertext to anyone who lacks the per-blob key.
- It does not bind the URL to a specific viewer. Anyone holding the
  URL within the embedding origin can dereference it.
- It does not prevent replay. A leaked URL is valid until `exp`.
  Operators who need stronger guarantees should set short TTLs
  (minutes, not days) and rely on revocation for incident response.
- It does not authenticate write requests. Signed URLs are read-only.
  Writes require a bearer token.

## Compatibility notes

- The format is intentionally similar to AWS S3 v4 query-string
  signing, with the simplification that we do not include the
  request method (signed URLs are GET-only) and we do not
  canonicalize headers (we use a separate `aud` field instead).
- Implementations may pre-validate a `kid` parameter against a
  short denylist of obviously-revoked keys before reaching the DB,
  for performance, as long as the canonical six-step flow runs on
  any kid that survives that pre-check.

## Test vectors

Implementations must pass the following vectors. The HMAC key is
the ASCII string `test-key-do-not-use-in-production` (33 bytes).

| canonical | sig (base64url) |
|---|---|
| `/blob/bafy/0\n0\n\n` | (placeholder — to be filled in Phase 1 alongside an authoritative test fixture) |

(A complete vector table will be generated by `internal/auth/testdata/vectors.txt`
in Phase 1; the spec-level test here ensures conformance.)
