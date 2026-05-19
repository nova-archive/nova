# IPFS Import Rules

Status: **Phase 0 — normative.** `internal/ipfs` must conform exactly.

## Purpose

Two conforming Nova implementations must produce **bit-identical
CIDs** for the same envelope bytes. Without that determinism,
content-addressed federation, deduplication, possession audits,
and any future proof-of-storage scheme fall apart.

This spec pins down every Kubo import parameter that affects the
resulting CID. Implementations have no freedom on these knobs;
operator configuration cannot override them.

## Required parameters

When importing the encryption envelope (or, in `public_archival`
collections, the plaintext bytes) into the local Kubo daemon,
`internal/ipfs` MUST use exactly:

| Parameter             | Required value                     | Why |
|---|---|---|
| CID version           | `v1`                               | v0 is sha256-only, base58btc; v1 supports modern multibase and codec choices |
| Multihash             | `sha2-256`                         | Universal support; aligns with the envelope's hash assumptions |
| Multibase             | `base32` (lowercase)                | URL-safe, case-insensitive in DNS, the IPFS default for v1 |
| Codec (small blobs)   | `raw` (0x55) when bytes ≤ 1 MiB   | Single-block; no UnixFS wrapping overhead |
| Codec (large blobs)   | `dag-pb` with UnixFS-1 file node   | Multi-block balanced DAG |
| Chunker               | `size-262144` (256 KiB fixed)      | Deterministic; no Rabin/Buzhash/content-defined chunking |
| Raw leaves            | `true`                             | Leaf blocks are raw codec, parent is dag-pb (only when codec = dag-pb) |
| Layout                | `balanced`                         | Trickle layout produces different CIDs for the same bytes |
| Maximum link count    | 174 (Kubo's UnixFS-1 default)      | Match Kubo's default to avoid divergent DAG shapes |

Threshold: **1 MiB** (1,048,576 bytes). At or below threshold the
envelope is stored as a single raw block (codec = `raw`); above
threshold it is chunked into 256 KiB fixed-size blocks under a
dag-pb UnixFS file node with `raw_leaves = true` and balanced
layout.

## Implementation

The Kubo Go API call equivalent:

```go
import (
    "github.com/ipfs/kubo/core/coreapi"
    "github.com/ipfs/kubo/core/coreapi/options"
)

api := coreapi.New(node)
path, err := api.Unixfs().Add(
    ctx,
    files.NewBytesFile(envelopeBytes),
    options.Unixfs.CidVersion(1),
    options.Unixfs.Hash(mh.SHA2_256),
    options.Unixfs.RawLeaves(true),
    options.Unixfs.Chunker("size-262144"),
    options.Unixfs.Layout(options.BalancedLayout),
    options.Unixfs.Pin(true),
)
```

For envelopes ≤ 1 MiB the implementation MAY shortcut by writing a
single raw block directly:

```go
block, err := blocks.NewBlockWithCid(envelopeBytes, raw_cid)
api.Block().Put(ctx, block, options.Block.Format("raw"))
```

Both paths must produce the same CID for the same bytes when the
shortcut threshold is honored uniformly.

## Manifest persistence

Every blob that lands in `data_encryption_keys`/`blobs` also lands
in `blob_manifests` and `blob_blocks`:

```sql
INSERT INTO blob_manifests
  (cid, cid_version, hash_alg, codec, chunker, plaintext_size,
   envelope_size, block_count, merkle_root)
VALUES
  ($1, 1, 'sha2-256', $2, 'size-262144', $3, $4, $5, $6);

INSERT INTO blob_blocks (blob_cid, block_cid, block_index, block_size)
VALUES (...) -- one row per block in import order
```

For single-block (raw) blobs: `block_count = 1`, `merkle_root` is
NULL (the blob CID is the root), one `blob_blocks` row indexed `0`.

For multi-block blobs: `merkle_root` is the dag-pb root CID
(equal to the blob CID), `block_count` is the leaf-block count,
one `blob_blocks` row per leaf block in DAG-traversal order.

This manifest is what makes Phase 2 possession audits possible.
The coordinator can challenge a donor for "block index 17 of CID
X with nonce Z" and verify the response against the stored
`block_cid`.

## What MUST NOT vary

- Operator configuration cannot override any value in this spec.
- The shortcut threshold (1 MiB) is fixed; it is not a tunable.
- The chunker is `size-262144` always; content-defined chunking
  (Rabin, Buzhash) is forbidden because it produces different CIDs
  for the same bytes depending on which IPFS implementation is
  importing.
- Trickle layout is forbidden.
- Custom UnixFS metadata fields are forbidden.

## What MAY vary

- The Kubo daemon version (within a stable major release line).
  Kubo guarantees CID stability for these parameters across
  minor versions.
- The transport-level details of how blocks are pushed to other
  nodes (this is the federation protocol's domain, not IPFS
  import's; see `FEDERATION_PROTOCOL.md` § "Repair transport").
- Future versions of this spec may relax the threshold or
  introduce streaming-AEAD codecs; such changes get a new
  envelope `version` byte and migration tooling.

## Verification

The Phase 1 integrity audit (see `INTEGRITY_AUDIT.md`) re-imports
sampled envelopes from the local blockstore and verifies the
recomputed CID matches the stored CID. Drift indicates a bug in
either this spec's implementation or in the Kubo version's CID
stability.

## Cross-references

- `docs/specs/ENCRYPTION_ENVELOPE.md` — the envelope this spec
  imports.
- `docs/specs/DATA_MODEL.sql` — `blob_manifests`, `blob_blocks`.
- `docs/specs/INTEGRITY_AUDIT.md` — Phase 1 local re-import
  verification.
- `docs/specs/POSSESSION_AUDIT.md` — Phase 2 donor block challenges
  that reference `blob_blocks.block_cid`.
