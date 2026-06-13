# Encryption Envelope

Status: **Phase 0 v3 — normative.** `internal/envelope` must conform exactly.

> **Amended by P2-M0 (2026-06-13)** — the v2 streaming-AEAD **record/DAG layout
> (D2) and per-chunk AAD construction (D3) are deferred to P2-M8** (golden
> vectors + crypto review): the "chunk N == block N" guarantee and the
> CID-in-AAD scheme in the v2 section below are **superseded, not settled**. The
> v2 Range benefit is reframed to **bounded-memory authenticated Range
> decryption at the origin** (D12), not ciphertext-serving CDN edges. The v1
> single-shot format is unchanged. See
> `docs/superpowers/specs/phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md`.

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
3. Master-key versioning and rotation (implemented in M10 (`internal/masterkey`)).
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

### Rotation procedure (implemented in M10 (`internal/masterkey`))

**Precondition.** The new master key must be loaded into the coordinator
*before* rotation is triggered. Deploy v2 to the secret mount (e.g.
`NOVA_MASTER_KEY_V2_FILE` or `/run/secrets/master-key-v2`), keep v1
present, set `NOVA_MASTER_KEY_ACTIVE=v2`, and **restart** the coordinator.
On boot the keystore loads both v1 and v2; new uploads already wrap DEKs
under v2. The rotation command then drains all remaining v1-wrapped rows
to v2.

**Invariant.** `rotate-master` requires `to_version == NOVA_MASTER_KEY_ACTIVE`.
This is enforced by the endpoint. If `to` is not the active label the
endpoint refuses with `400 to_not_active` and instructs the operator to
set the env var and restart first.

**CLI:**

```
novactl keys rotate-master --from v1 --to v2 [--no-confirm]
```

The CLI prompts for confirmation (bypassed by `--no-confirm`), then polls
`GET /api/v1/admin/keys/rotation-status` printing remaining DEK and signing
key counts until the rotation completes or stalls.

**Algorithm:**

1. Endpoint validates: `to == active label`, both labels loaded, `from` has a
   `master_key_versions` row, `from` is not retired, no other version already
   `rotating`. Returns `400` or `409` on any violation.
2. Mark the `from` version's `master_key_versions` row `state = 'rotating'`
   (atomic; fails if any other version is already `rotating`). The `rotating`
   state marks the **version row**, not individual DEK rows — DEK `state` stays
   `'active'` throughout.
3. Endpoint returns `202 {from, to, total_deks, total_signing_keys}` and
   starts the background worker pool non-blocking.
4. Worker claims batches of `data_encryption_keys` rows where
   `master_key_version_id = from.id AND state IN ('active','rotating')`
   (`FOR UPDATE SKIP LOCKED`, default 256 rows/batch). For each row:
   unwrap `wrapped_key` with the old `MK`; re-wrap with the new `MK`
   (fresh wrap nonce); perform one **atomic, version-guarded `UPDATE`**:
   ```sql
   UPDATE data_encryption_keys
      SET wrapped_key = $new_wrapped, master_key_version_id = $new_id
    WHERE id = $row_id AND master_key_version_id = $old_id;
   ```
   `wrapped_key` and `master_key_version_id` flip together in a single
   statement. A concurrent reader always sees a consistent `(wrapped, version)`
   pair. The `WHERE master_key_version_id = $old_id` guard makes each update
   **idempotent and race-safe**: a re-run or a concurrent worker matches 0 rows
   on an already-migrated row. `legal_hold` DEKs are re-wrapped normally
   (re-wrap is not a shred; the `no_shred_under_legal_hold` CHECK is unaffected).
   An inter-batch pace (default 50 ms, `NOVA_MASTER_KEY_REWRAP_PACE_MS`) keeps
   WAL and I/O headroom.
5. After all DEKs drain, re-wrap `signing_keys` with
   `master_key_version_id = from.id AND state IN ('active', 'retired')` — the
   active key **and all non-shredded retired keys** (a retired-but-not-yet-shredded
   key still holds real bytes and still verifies signed URLs; omitting it would
   orphan it). `shredded` signing keys are skipped: their `wrapped_key` is
   already zeroed. The same atomic guarded `UPDATE` applies. After re-wrapping,
   the signing-key cache is invalidated.
6. Mark the `from` version `state = 'retired'`, `retired_at = now()`.
7. **Operator confirms** `novactl keys status` shows `from` retired with 0
   referencing rows, then removes the old `MK` from env/mounts on the **next
   deploy**.

Rotation is online: reads continue against whichever version a row is currently
wrapped under (the keystore resolves version by `master_key_version_id`, and both
keys are loaded). There is no read-path downtime. A 1 M-blob deployment drains
in a few minutes on commodity hardware.

**Resume on restart.** If the coordinator restarts mid-rotation, `ResumeIfRotating`
picks up any `rotating` version on boot and continues draining. The guarded
`UPDATE` makes resumption idempotent. If the `from` key was prematurely removed,
the rotation **stalls**: the version stays `rotating`, `/readyz` degrades (readiness,
not liveness — a restart cannot conjure a missing key), and `rotation-status.stalled`
is `true`. The operator must restore the `from` key and restart to resume.

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

Status: **planned design sketch — record/DAG layout + AAD authoritative in
P2-M8.** This section reserves the wire-format slots and constrains Phase 1
implementations to leave v2 room. The exact encrypted-record ↔ IPFS-block
mapping (D2) and the per-chunk AAD commitment (D3) are settled in **P2-M8** with
golden vectors and a focused crypto review; the sketches below are illustrative,
not normative, until then.

### Goals

- **Range-serveable encrypted blobs.** HTTP 206 with the correct
  `Content-Range` for any byte range, decrypting only the chunks
  that cover the range.
- **Bounded-memory authenticated Range decryption at the origin (D12).** The
  coordinator fetches and decrypts only the records covering the requested range
  and returns a plaintext `206` — without buffering the whole object. (The
  earlier "CDN edges serve individual ciphertext chunks" framing is dropped:
  Nova's default read path decrypts at the coordinator, so a ciphertext-caching
  edge would require a Nova-aware intermediary and is not the default.)
- **First-byte latency independent of object size.** The gateway
  decrypts the first relevant chunk and starts streaming
  immediately; subsequent chunks decrypt in parallel with delivery.
- **Federation reuse.** Donors fetch and serve whole IPFS blocks as today, so
  donor-to-donor repair, possession audits, and partial-read serving share the
  same per-block infrastructure. **(D2 — the earlier "chunk N == block N"
  guarantee is superseded.)** A 40-byte header + ciphertext + 16-byte tag per
  record cannot align with fixed 256 KiB UnixFS leaves, so the authoritative
  encrypted-record ↔ block mapping is settled in **P2-M8** (e.g. a custom DAG
  with one raw leaf per record, a fixed-size encrypted record, or a
  ciphertext-offset → block map in `blob_manifests`).

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

Note (D3 — **deferred to P2-M8**): binding per-chunk AAD to the final `cid` is
circular — the CID is computed over ciphertext that already contains the tags.
P2-M8 resolves this by binding AAD to a **canonical header commitment**
(`hash(canonical_header) ‖ chunk_index ‖ total_chunks ‖ plaintext_len`), not the
final CID; the content address authenticates the whole object while per-chunk
AAD prevents reordering/substitution. Exact construction + vectors land in P2-M8
under crypto review. The `cid_v1_prefix`-in-AAD sketch above is **superseded**.

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

### What requires deliberation — authoritative in P2-M8

These items are settled in **P2-M8** (the streaming-envelope design milestone)
with golden vectors and a focused crypto review, before any v2 write/read code:

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
