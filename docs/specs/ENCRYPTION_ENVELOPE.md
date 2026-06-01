# Encryption Envelope

Status: **Phase 0 v2 — normative.** `internal/envelope` must conform exactly.

## Purpose

Per-blob symmetric encryption is the architectural foundation of
donor-blind storage. Donor pinning nodes hold the encrypted bytes of
this envelope and never see plaintext. The coordinator's read gateway
is the only component that decrypts.

> **Trust-model note.** This spec achieves donor-blindness, not
> operator-blindness. The coordinator decrypts plaintext on every read
> and on transform; the operator's master key is process-resident.
> Operators must run the coordinator under host-level security
> commensurate with that responsibility. See `docs/THREAT_MODEL.md`.

This spec covers four layers:

1. The on-IPFS envelope wire format.
2. Per-blob key generation and key wrapping with the operator master key.
3. Master-key versioning and rotation (Phase 1 deliverable).
4. Crypto-shredding for deletion, with legal-hold gates.

## Envelope wire format

A blob is stored in IPFS as the following byte layout. Header first.

| Offset | Length | Field            | Notes                               |
|-------:|-------:|------------------|-------------------------------------|
|     0  |     4  | `magic`          | ASCII `NOVE` (0x4E 0x4F 0x56 0x45)  |
|     4  |     1  | `version`        | `0x01` for this spec                |
|     5  |     1  | `algorithm`      | `0x01` = XChaCha20-Poly1305         |
|     6  |     2  | `reserved`       | `0x0000`; readers reject if non-zero |
|     8  |    24  | `nonce`          | XChaCha extended nonce, CSPRNG-random |
|    32  |    n   | `ciphertext`     | AEAD ciphertext of the plaintext    |
|  32+n  |    16  | `tag`            | Poly1305 authentication tag         |

Total envelope size = `32 + plaintext_length + 16` bytes.

The CID stored in `blobs.cid` is the CID of the **entire envelope** —
header, ciphertext, and tag together. Decryption therefore requires
loading the full envelope.

The `nonce` is generated fresh per upload from a cryptographically
secure RNG. XChaCha's 192-bit nonce space makes random-nonce
collisions astronomically unlikely; **deterministic nonces are
forbidden** because they would let two identical plaintexts produce
identical ciphertexts and identical CIDs, leaking equality information
to anyone holding the bytes.

### Decrypt flow

1. Fetch envelope bytes for the CID (from local Kubo, federation, or cache).
2. Verify `len(envelope) >= 48` (header + tag minimum).
3. Verify `magic == "NOVE"`, `version == 0x01`, `algorithm == 0x01`,
   `reserved == 0x0000`. Any mismatch returns `envelope_unsupported`.
4. Look up `blobs.encryption_key_id` by CID; if `NULL`, the blob is in
   public-archival mode and the envelope bytes are actually plaintext —
   skip decryption.
5. Look up the active `keys` row by id; if `state = 'shredded'`,
   return `410 Gone` to the caller.
6. Unwrap the per-blob key with the operator master key (see below).
7. Run `XChaCha20-Poly1305-Decrypt(per_blob_key, nonce, ciphertext, tag)`.
   The tag check is constant-time inside the AEAD primitive.
8. Stream the plaintext to the caller.

The per-blob key is held in process memory only for the duration of
the request. It is never written to disk, logs, or any caching layer.

## Per-blob key generation and wrapping

Per-blob keys are 256-bit symmetric secrets, used directly with
XChaCha20-Poly1305 for the envelope. They live in
`data_encryption_keys` (separate table from signing keys; see § "Key
purpose split").

### Generate

```
per_blob_key := CSPRNG(32 bytes)
```

### Wrap with master key

The operator master key (`MK`) wraps the per-blob key so the database
can store the wrapped value. Each wrapping records which master-key
version was used; rotation depends on this record.

```
wrap_nonce                            := CSPRNG(24 bytes)
wrapped_payload                       := XChaCha20-Poly1305-Encrypt(MK, wrap_nonce, AAD = "", plaintext = per_blob_key)
                                      := ciphertext_of_key (32 bytes) || tag (16 bytes)
data_encryption_keys.wrapped_key      := wrap_nonce || wrapped_payload
data_encryption_keys.master_key_version_id := <id of currently-active master_key_versions row>
```

`wrapped_key` is therefore exactly **72 bytes** while
`state IN ('active', 'rotating')`.

### Unwrap

```
master_key_version  := SELECT * FROM master_key_versions WHERE id = data_encryption_keys.master_key_version_id
MK                  := load NOVA_MASTER_KEY for this version  // see § "Master key versioning"
wrap_nonce          := wrapped_key[0:24]
wrapped_payload     := wrapped_key[24:72]
per_blob_key        := XChaCha20-Poly1305-Decrypt(MK, wrap_nonce, AAD = "", wrapped_payload)
```

## Master key versioning

The operator master key is loaded from the environment variable
`NOVA_MASTER_KEY`, hex-encoded (64 hex characters → 32 bytes). Each
distinct master key value over the deployment's lifetime is a
**master key version**, tracked in the `master_key_versions` table.

Every wrapped key (data and signing) records the master-key version
that wrapped it. This makes rotation tractable: the coordinator
walks `data_encryption_keys` and `signing_keys` rows whose
`master_key_version_id` references the retiring version, unwraps with
the old `MK`, re-wraps with the new `MK`, updates the row's
`master_key_version_id`, and atomically commits each row.

Multi-version environment loading: during a rotation, both the new
and old `MK` values are present in process memory. They are
distinguished by version label:

```
NOVA_MASTER_KEY_V1=<old hex>
NOVA_MASTER_KEY_V2=<new hex>
NOVA_MASTER_KEY_ACTIVE=v2
```

The coordinator loads every set version, picks `NOVA_MASTER_KEY_ACTIVE`
as the default for new keys, and uses each row's
`master_key_version_id` to choose the correct unwrapper.

### Secret resolution chain (M6.1)

Each `NOVA_MASTER_KEY_<LABEL>` is resolved through a three-step
precedence chain so the master key never has to sit in the process
environment — Docker / Kubernetes secret mounts and per-label
`_FILE` redirects are both first-class:

```
NOVA_MASTER_KEY_<LABEL>          (inline hex; first precedence)
  → NOVA_MASTER_KEY_<LABEL>_FILE (path to a file holding the hex)
  → /run/secrets/master-key-<label>  (default secret-mount path)
```

The lowest-precedence leaf is lowercased because Linux paths are
case-sensitive. The active label (`NOVA_MASTER_KEY_ACTIVE`) is
always resolved through the full chain, so the common case — drop
`/run/secrets/master-key-v1`, set `NOVA_MASTER_KEY_ACTIVE=v1` —
works with no key material in the environment.

Additional (rotation) labels are declared by an inline value or a
`_FILE` env. A declared label that resolves from no source, or a
set-but-unreadable `_FILE`, is **fatal at startup** — never
silently skipped, because its wrapped blobs would become
permanently unreadable. The active label is also fatal when it
resolves from no source, with the same reasoning.

The `ACTIVE` and `FILE` pseudo-labels (and the `_FILE` suffix on
any other label) are stripped from the candidate-label set before
resolution, so typo'd forms like `NOVA_MASTER_KEY_ACTIVE_FILE` or
`NOVA_MASTER_KEY_FILE_FILE` cannot leak in as phantom labels.

The same precedence chain is used for the OIDC signing key and any
other secret loaded through `internal/config.ResolveSecret`. See
`THREAT_MODEL.md` boundary ③ and `docs/REVIEW_2026_05_25.md` § C3.

Constraints:

- **MUST be at least 256 bits of entropy** per version. The
  coordinator refuses to start with any version shorter than 32 bytes.
- **MUST NOT be persisted to the database.** Each `MK` value exists
  only in process memory and the operator's secret-management system.
- **MUST be backed up out-of-band.** Loss of all active `MK` versions
  is equivalent to permanent loss of every blob in the federation.
  Document this prominently in `OPERATOR_CHECKLIST.md`.

### Rotation procedure (Phase 1 deliverable)

```
novactl keys rotate-master \
    --from-version v1 \
    --to-version v2 \
    --new-key-env NOVA_MASTER_KEY_V2
```

Algorithm:

1. INSERT new row in `master_key_versions` with `state = 'active'`.
2. Mark old row `state = 'rotating'`.
3. For each `data_encryption_keys` row with `master_key_version_id = old.id` and `state = 'active'`:
   a. Mark row `state = 'rotating'`.
   b. Unwrap `wrapped_key` with old `MK`.
   c. Re-wrap with new `MK`, generating a fresh wrap nonce.
   d. UPDATE `wrapped_key`, `master_key_version_id = new.id`, `state = 'active'`.
4. Same for `signing_keys`, but with the filter `state IN ('active', 'retired')`
   — active keys **and** retired keys still inside their grace window must be
   re-wrapped, because both still verify signed URLs (see
   `docs/specs/SIGNED_URL_FORMAT.md` § "Key rotation"). `shredded` signing keys
   are skipped: their `wrapped_key` is already zeroed. Missing the `retired`
   rows would orphan in-grace keys and break signed-URL verification for URLs
   minted just before the rotation. (Signing-key wrapping lands in M7; this
   re-wrap step is realised in M10.)
5. Mark old row `state = 'retired'`, set `retired_at = now()`.
6. Operator removes the old `MK` env var on next deploy.

Rotation is online: reads continue against the old version until each
row is updated, then continue against the new version. There is no
read-path downtime. A 1 M-blob deployment rotates in a small number
of minutes on commodity hardware.

## Crypto-shredding

Deletion is implemented as crypto-shredding the per-blob key — but
only when no legal hold prevents it.

### Pre-conditions

The shred procedure refuses to run when:

- The target row's `legal_hold = true`. Severe-content preservation
  flows set this and the shred operation must wait for an operator
  with the appropriate role to clear the hold. See
  `docs/legal/SEVERE_CONTENT_PROCEDURE.md`.
- The blob's `state` is not yet `'tombstoned'` and no scheduled
  tombstone job is running. The shred is the consequence of a
  state transition, not its trigger.

### Procedure

```sql
-- Verify pre-condition (application layer also checks):
SELECT legal_hold INTO STRICT v_hold
  FROM data_encryption_keys WHERE id = $1;
IF v_hold THEN RAISE EXCEPTION 'cannot shred: legal_hold = true'; END IF;

UPDATE data_encryption_keys
   SET state       = 'shredded',
       shredded_at = now(),
       wrapped_key = decode(repeat('00', 72), 'hex')
 WHERE id = $1;
```

Postgres autovacuum reclaims the old row's bytes within its normal
schedule (minutes to hours). The 32-byte plaintext per-blob key was
never persisted; it is gone once the encrypting request returned.
**The ciphertext on donor disks may persist for `max_offline_window`
(default 30 days) but is computationally unrecoverable** without the
per-blob key.

The shred is paired with:

- `blobs.state = 'tombstoned'`
- An `unpin` broadcast to all donor nodes (see `FEDERATION_PROTOCOL.md`)
- A cascade to all child derivatives (their state and their keys)
- An audit-log entry
- A `signed_url_revocations(kind='cid', value={cid})` row so any
  outstanding signed URLs are immediately invalidated

### What crypto-shredding actually achieves

Crypto-shredding makes donor-held ciphertext **computationally
unreadable**, assuming the per-blob key is not recoverable from
backups, logs, memory dumps, or other side channels under the
operator's control. It is one component of an erasure procedure,
not a complete one.

Crypto-shredding **does not, by itself**, address:

- CDN plaintext caches (must be purged via the operator's CDN integration)
- Browser caches on viewers' devices
- Reverse-proxy access logs
- Postgres WAL or backup retention
- Operating-system temporary files from upload or transform pipelines
- Plaintext exports the operator may have generated
- Derivative blobs (these must be tombstoned and shredded explicitly;
  see `PRODUCT_MODULE_INTERFACE.md`)
- Moderation queues holding plaintext copies
- Evidence-preservation obligations (see `SEVERE_CONTENT_PROCEDURE.md`)

GDPR Article 17 erasure obligations and DMCA takedown obligations
are satisfied by the **complete erasure procedure** the operator
runs, of which crypto-shredding is one technical step. The
operator's procedure must address the items above. See
`docs/THREAT_MODEL.md` § "Acknowledged residual risks" and
`docs/legal/OPERATOR_CHECKLIST.md` for the full picture.

## Public-archival opt-out

A collection explicitly marked `public_archival = true` (column on
the `collections` table; constrained to require `visibility = 'public'`)
MAY opt out of envelope encryption. In that mode:

- `blobs.encryption_key_id` is `NULL`.
- The bytes pushed to IPFS are the plaintext directly (no envelope
  header).
- The CID is the CID of the plaintext.
- The read gateway streams bytes verbatim with no decrypt hop, which
  is materially cheaper at the gateway and CDN-friendly without a
  Nova-aware proxy.

This trade is **not** exposed by `nova-image` or any other product
layer that handles personal or potentially-infringing content. The
storage core's Go config struct does not surface a global "encryption
off" toggle; the only path is `Collection.PublicArchival = true`,
and the SQL `CHECK` constraint refuses the flag on a non-public
collection. Intended for the future `nova-archive` product layer
hosting genuinely open data.

## Key purpose split (v2)

The original Phase 0 schema had a single `keys` table holding both
per-blob data-encryption keys and HMAC signing keys for signed URLs.
That conflated lifecycles — data keys are created and shredded per
upload; signing keys rotate on a schedule with grace windows.

v2 splits them:

- `data_encryption_keys` — per-blob XChaCha20-Poly1305 keys with
  `legal_hold` flag, owned by `blobs.encryption_key_id`.
- `signing_keys` — HMAC-SHA256 signing keys for signed URLs, keyed
  by `kid` (the public identifier embedded in URLs), with
  `active_from` / `retire_after` grace-window timestamps.
- `master_key_versions` — operator master-key history, referenced by
  both tables.

See `docs/specs/SIGNED_URL_FORMAT.md` for the signing-key lifecycle.

## What this spec deliberately does not specify (for v1)

- **AAD / additional authenticated data on v1.** The v1 envelope
  reserves no AAD field. v2 (see below) reintroduces per-chunk AAD
  binding `chunk_index || total_chunks || cid` for streaming.
- **Streaming AEAD in v1.** v1 is single-shot; multi-gigabyte blobs
  that exceed memory limits are not in scope for v1. **HTTP `Range`
  requests on v1 envelopes return `416`** unless the blob is in a
  `public_archival` collection. v3.1 amendment: streaming AEAD is
  planned for Phase 2 alongside federation (was previously Phase 6+);
  see § "Planned v2: Streaming-AEAD" below.
- **Hardware key storage.** HSMs and KMS integration are out of scope.
  Operators with such requirements can wrap `NOVA_MASTER_KEY`
  loading to fetch from their KMS at boot.
- **Key derivation from CID.** Tempting (no key table) but kills
  per-blob crypto-shredding. Out of scope.

## Planned v2: Streaming-AEAD (Phase 2 deliverable)

The v1 envelope above is single-shot: the gateway must AEAD-verify
the entire ciphertext before any plaintext byte streams to the
caller. This blocks Range requests, defeats CDN partial-object
caching, and produces unacceptable TTFB for large objects (audio,
video, large image archives). v2 fixes this with chunk-authenticated
streaming AEAD while preserving every Tier 1 commitment (donor-
blindness, deterministic CIDs, master-key wrapping, per-blob
crypto-shredding).

Status: **planned design sketch.** The authoritative v2 spec lands
when Phase 2 begins. This section reserves the wire-format slots
and constrains Phase 1 implementations to leave v2 room.

### Goals

- **Range-serveable encrypted blobs.** HTTP 206 with the correct
  `Content-Range` for any byte range, decrypting only the chunks
  that cover the range.
- **CDN-compatible partial-object caching.** Edges can store and
  serve individual ciphertext chunks without coordinating with the
  origin per byte range.
- **First-byte latency independent of object size.** The gateway
  decrypts the first relevant chunk and starts streaming
  immediately; subsequent chunks decrypt in parallel with delivery.
- **Federation reuse.** Streaming chunks align with IPFS block
  boundaries (256 KiB, the chunker we already mandate). Chunk N ==
  block N. Donors fetch and serve whole IPFS blocks as today;
  donor-to-donor repair, possession audits, and partial-read
  serving all share the same per-block infrastructure.

### Wire format (sketch)

| Offset       | Length  | Field            | Notes |
|-------------:|--------:|------------------|-------|
|         0    |    4    | `magic`          | ASCII `NOVE` |
|         4    |    1    | `version`        | `0x02` (v2) |
|         5    |    1    | `algorithm`      | `0x02` = XChaCha20-Poly1305-Streaming |
|         6    |    1    | `chunk_size_log2`| Log-base-2 of chunk size in bytes; `18` = 256 KiB |
|         7    |    1    | `flags`          | bit 0 = "last chunk has final-chunk marker"; reserved otherwise |
|         8    |    8    | `total_chunks`   | uint64 big-endian; number of chunks |
|        16    |   24    | `base_nonce`     | 192-bit random base; chunk nonces derive from this + counter |
|        40    |    n_1  | `chunk_1_ct`     | First chunk ciphertext |
|   40 + n_1   |    16   | `chunk_1_tag`    | Poly1305 tag for chunk 1 |
|       ...    |    ...  | ...              | repeat per chunk |
|             |    1    | `final_marker`   | for the last chunk, before its tag: `0xFF` |

Header is 40 bytes (vs. 32 in v1). Per-chunk overhead is the
16-byte Poly1305 tag plus an optional final-chunk byte. The CID is
still the CID of the entire envelope — bit-identical determinism
preserved.

### Chunk encryption

For chunk index `i` in `[0, total_chunks)`:

```
nonce_i := XOR(base_nonce, big_endian_uint192(i))   # XChaCha 192-bit nonce
aad_i   := chunk_index || total_chunks || cid_v1_prefix
ct_i || tag_i := XChaCha20-Poly1305-Encrypt(per_blob_key, nonce_i, aad_i, plaintext_chunk_i)
```

The chunk's nonce never collides because each chunk has a distinct
counter, and the base_nonce is per-blob random. The AAD binds the
chunk to its position and to the eventual CID, so a tampered
envelope that swaps two chunks fails authentication on at least one.

Note: the `cid_v1_prefix` in AAD is the CID's multihash bytes (not
the human-readable CID string), computed and committed *before* the
last chunk's tag is finalized. The CID itself is computed over the
envelope bytes after encryption; the prefix used in AAD comes from
an intermediate commitment scheme. The full spec works this out in
Phase 2.

### Range read path

1. Receive `GET /blob/{cid}` with `Range: bytes=A-B`.
2. Look up `blob_manifests` and `blob_blocks` for the CID; compute
   which block indices cover `[A, B]`.
3. Fetch only those blocks from local Kubo (one block put-get per
   chunk). Per-block fetch latency is bounded by Kubo's blockstore
   read; no full-envelope load.
4. Decrypt each chunk: derive `nonce_i`, verify `tag_i`, recover
   plaintext_chunk_i. If any chunk fails, abort with `502`.
5. Stream plaintext for the requested byte range, trimming the
   first and last chunks to the exact `[A, B]` boundaries.

### Phase 1 implementation constraints

To make v2 a drop-in addition rather than a refactor, Phase 1
ships:

- A `Codec` interface in `internal/envelope` with `Encrypt(plaintext,
  key) → envelope` and a streaming-aware `Decrypter(envelope, key)
  → io.ReadSeeker` (single-shot for v1; partial-decrypt for v2).
- A version-dispatching decoder that reads the envelope's `version`
  byte at offset 4 and routes to the appropriate codec.
- An `envelope_version` field on the JSON `Blob` schema and an
  `X-Nova-Envelope-Version` response header on `/blob/{cid}` and
  `/i/{cid}` so CDNs and clients learn the format from the
  response.
- `blob_manifests.codec` stays free-form text so v2 can record
  `"chunked-aead-v1"` without DDL changes.

### What v2 does not change

- The CID is still the CID of the entire envelope. Bit-identical
  determinism.
- Donor-held bytes remain opaque ciphertext.
- Per-blob keys, master-key wrapping, master-key rotation
  semantics, and crypto-shredding are unchanged.
- v1 envelopes remain decryptable forever. The version byte at the
  envelope header dispatches.
- Tier 1 commitments (donor-blind, single-coordinator, deterministic
  CIDs) are unchanged.

### What requires deliberation before Phase 2 implementation

- Authoritative test vectors covering chunk-boundary edge cases,
  final-chunk marker presence/absence, and AAD substitution attacks.
- The exact AAD CID-commitment scheme (the chicken-and-egg between
  computing the CID and using it as AAD).
- Whether to support a chunk size other than 256 KiB. Lock to 256
  KiB initially; broaden only if a real consumer needs it.
- Range-request error semantics for partially-corrupt envelopes:
  return a partial response with the verified prefix, or always
  fail closed.

## Test vectors

Authoritative vectors will be generated by `internal/envelope/testdata/`
in Phase 1 alongside the production implementation. Phase 0 cross-
implementation testing is unnecessary because there is exactly one
implementation.
