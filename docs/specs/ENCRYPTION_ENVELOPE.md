# Encryption Envelope

Status: **Phase 0 — normative.** `internal/envelope` must conform exactly.

## Purpose

Per-blob symmetric encryption is the architectural foundation of
donor-blind storage. Donor pinning nodes hold the encrypted bytes of
this envelope and never see plaintext. The coordinator's read gateway
is the only component that decrypts.

This spec covers three layers:

1. The on-IPFS envelope wire format.
2. Per-blob key generation and key wrapping with the operator master key.
3. Crypto-shredding for deletion.

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
XChaCha20-Poly1305 for the envelope.

### Generate

```
per_blob_key := CSPRNG(32 bytes)
```

### Wrap with master key

The operator master key (`MK`) wraps the per-blob key so the database
can store the wrapped value.

```
wrap_nonce        := CSPRNG(24 bytes)
wrapped_payload   := XChaCha20-Poly1305-Encrypt(MK, wrap_nonce, AAD = "", plaintext = per_blob_key)
                  := ciphertext_of_key (32 bytes) || tag (16 bytes)
keys.wrapped_key  := wrap_nonce || wrapped_payload
```

`keys.wrapped_key` is therefore exactly **72 bytes** while
`state = 'active'`.

### Unwrap

```
wrap_nonce        := keys.wrapped_key[0:24]
wrapped_payload   := keys.wrapped_key[24:72]
per_blob_key      := XChaCha20-Poly1305-Decrypt(MK, wrap_nonce, AAD = "", wrapped_payload)
```

## Master key

The operator master key is loaded from the environment variable
`NOVA_MASTER_KEY`, hex-encoded (64 hex characters → 32 bytes).

Constraints:

- **MUST be at least 256 bits of entropy.** The coordinator refuses
  to start with a key shorter than 32 bytes.
- **MUST NOT be persisted to the database.** It exists only in
  process memory and the operator's secret-management system.
- **MUST be backed up out-of-band.** Loss of `MK` is equivalent to
  permanent loss of every blob in the federation. Document this
  prominently in `OPERATOR_CHECKLIST.md`.
- **SHOULD be rotated.** Annual rotation is best practice; immediate
  rotation on suspected compromise is mandatory. Rotation requires
  re-wrapping every active `keys` row, which is an `O(N)` migration
  shipped as `novactl keys rotate-master --new-key=...`. Phase 1
  ships only forward use of `NOVA_MASTER_KEY`; the rotation
  migration tool lands in Phase 5.

## Crypto-shredding

Deletion is implemented as crypto-shredding the per-blob key.

```sql
UPDATE keys
   SET state = 'shredded',
       shredded_at = now(),
       wrapped_key = decode(repeat('00', 72), 'hex')
 WHERE id = $1;
```

Postgres autovacuum reclaims the old row's bytes within its normal
schedule (minutes to hours). The 32-byte plaintext per-blob key was
never persisted; it is gone once the encrypting request returned.
**The ciphertext on donor disks may persist for `max_offline_window`
(default 30 days) but is unrecoverable** without the per-blob key.

The shred is paired with:

- `blobs.state = 'tombstoned'`
- An `unpin` broadcast to all donor nodes (see `FEDERATION_PROTOCOL.md`)
- An audit-log entry
- Optionally, a `signed_url_revocations` prefix `cid:{cid}` so any
  outstanding signed URLs are immediately invalidated even before the
  CID becomes unreadable

GDPR Article 17 erasure is satisfied at the moment of shred; the bytes
that may persist on donor disks are mathematically equivalent to
random noise and the operator no longer holds the means to read them.

## Public-archival opt-out

A collection explicitly marked `public_archival = true` (intended for
the future `nova-archive` product layer hosting open data) MAY opt out
of envelope encryption. In that mode:

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
off" toggle; the path is `Collection.PublicArchival = true` only.

## What this spec deliberately does not specify

- **AAD / additional authenticated data.** The envelope reserves no
  AAD field. If a future version needs to bind metadata into the
  authenticated section (e.g., to commit to MIME type), bump
  `version` and reserve part of the header for it.
- **Streaming AEAD.** All encryption and decryption is single-shot.
  Multi-gigabyte blobs that exceed memory limits are not in scope
  for Phase 1; Phase 6+ may introduce a streaming variant with a
  separate `algorithm` ID.
- **Hardware key storage.** HSMs and KMS integration are out of scope.
  Operators with such requirements can wrap `NOVA_MASTER_KEY`
  loading to fetch from their KMS at boot.
- **Key derivation from CID.** Tempting (no `keys` table) but kills
  per-blob crypto-shredding. Out of scope.

## Test vectors

Authoritative vectors will be generated by `internal/envelope/testdata/`
in Phase 1 alongside the production implementation. Phase 0 cross-
implementation testing is unnecessary because there is exactly one
implementation.
