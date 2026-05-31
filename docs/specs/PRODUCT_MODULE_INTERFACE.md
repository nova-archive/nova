# Product Module Interface

Status: **Phase 0 v2 — normative.** Defines how content-type-specific
product layers (`nova-image` first, future `nova-video`,
`nova-audio`, `nova-archive`, `nova-document`) plug into the
storage core (`nova-storage`).

## Purpose

The storage core is content-agnostic. It owns CIDs, encryption,
replication, federation, signed URLs, deletion, and audit. It does
not know what the bytes mean.

A product module is a Go package that registers itself with the
coordinator at boot and contributes:

1. **MIME-type validation** for uploads.
2. **Side-table metadata** (e.g., width, height, perceptual hash for
   images).
3. **Content-type-specific moderation hooks** (e.g., PDQ scan
   against StopNCII for images).
4. **URL routes** (e.g., `/i/{cid}/...` for image transforms).
5. **Optional read-time transforms** (e.g., govips-driven resize).
6. **Optional automatic format conversion** at upload (e.g., PNG →
   WebP for the spectral-bloat use case described in
   `nova-image`'s docs).

The interface is part of `pkg/coordinator`'s public API and follows
its semver discipline. Adding methods is a major-version change.
Adding non-required hooks via wrapper interfaces is a minor-version
change.

## Upload pipeline (v2 — single canonical ordering)

The original Phase 0 spec contradicted itself on whether `OnUpload`
ran before or after CID computation. The contradiction was real: with
random-nonce envelope encryption, the envelope CID does not exist
until after encryption. v2 resolves this with a single canonical
sequence and two distinct hooks.

```
1.  Receive upload into temp storage (tus chunks accumulate).
2.  Storage core: validate declared MIME / declared size / collection.
3.  Storage core: route to the product based on declared product
    type or matched MIME type.
4.  Product hook AnalyzeUpload(ctx, plaintext) → (Metadata, ScanResult, error)
       - product extracts metadata (e.g., width/height/perceptual hash)
       - product runs synchronous moderation scanners
       - product MAY return a transformed plaintext (e.g., PNG → WebP
         for nova-image's spectral-bloat mitigation) — see § "Format
         conversion"
       - moderation tombstone/quarantine short-circuits the pipeline
5.  Storage core: encrypt (transformed) plaintext into envelope
    using deterministic Kubo settings (see IPFS_IMPORT_RULES.md).
6.  Storage core: import envelope into local Kubo, obtain envelope CID.
7.  Storage core: write blob_manifests + blob_blocks rows for proof-readiness.
8.  DB transaction:
       INSERT blobs row (with metadata fields)
       INSERT product side-table row (keyed by CID)
       COMMIT
9.  Storage core: pin to local Kubo cluster.
10. Storage core: enqueue replication via the orchestrator.
11. Product hook OnCommitted(ctx, blob, metadata) (best-effort, async-ok)
       - generate derivatives, side-write thumbs, emit webhooks
       - any failure here does NOT roll back the upload
```

Step 4's `AnalyzeUpload` is the moderation/metadata gate. Step 11's
`OnCommitted` is the post-commit hook for asynchronous follow-on
work.

The plaintext stream passed to `AnalyzeUpload` is provided for the
duration of that call only. Products MUST NOT retain it. After
`AnalyzeUpload` returns, the storage core takes ownership of the
(possibly transformed) plaintext for encryption.

## Go interface

```go
// Package product, part of pkg/coordinator/product.
package product

import (
    "context"
    "io"
    "io/fs"

    "github.com/go-chi/chi/v5"
    "github.com/nova-archive/nova/pkg/coordinator/storage"
)

// Product is registered with the coordinator at boot. Each
// product implementation is a singleton per coordinator process.
type Product interface {
    // Name returns the canonical product identifier ("image",
    // "video", "archive", ...). Persisted in blobs.product.
    Name() string

    // AcceptedMimeTypes returns the MIME types this product
    // accepts at upload. Empty slice means "any MIME type".
    AcceptedMimeTypes() []string

    // AnalyzeUpload runs on plaintext before encryption. The product
    // extracts metadata, runs moderation scanners, and optionally
    // returns a transformed plaintext (e.g., re-encoded format) for
    // the storage core to encrypt and store.
    //
    // plaintext is provided for the lifetime of this call only;
    // the product MUST NOT retain it.
    //
    // If transformedPlaintext is non-nil, the storage core encrypts
    // and stores the transformed bytes. The original is discarded.
    //
    // If ScanResult.Action != "allow", the upload is rejected and
    // the storage core does not commit any state.
    AnalyzeUpload(
        ctx context.Context,
        upload *UploadContext,
        plaintext io.Reader,
    ) (metadata Metadata, scan *storage.ScanResult, transformedPlaintext io.Reader, err error)

    // OnCommitted is called after the storage core has committed the
    // blob and product side-table rows. Best-effort; errors here are
    // logged and metric-counted but do not roll back the upload.
    // Use this for async derivative generation, webhook emission,
    // and similar follow-on work.
    OnCommitted(ctx context.Context, blob *storage.Blob, metadata Metadata) error

    // OnDelete is called as part of the tombstone flow, before the
    // crypto-shred and unpin broadcast. The product cleans up
    // side-table rows and cascades to its own derivatives.
    OnDelete(ctx context.Context, blob *storage.Blob) error

    // RegisterRoutes mounts the product's read routes on the chi
    // router under its canonical prefix. The coordinator reserves
    // /blob/* and /api/v1/* for storage; products may use any
    // other prefix consistent with their Name (conventionally a
    // single-letter or short word, e.g., /i for images, /v for
    // videos).
    RegisterRoutes(r chi.Router)

    // Migrations returns the product's database migration
    // directory in embedded form. The coordinator runs all
    // products' migrations at boot, in registration order, and
    // a product whose migrations fail prevents startup.
    Migrations() (fs.FS, string) // (embedFS, subdir)
}

// UploadContext carries declared metadata from the upload request:
// declared MIME type, declared filename, target collection, owner.
type UploadContext struct {
    DeclaredMimeType string
    Filename         string
    CollectionID     *string
    OwnerID          *string
    SourceIP         string
}

// Metadata is product-specific. nova-image returns ImageMetadata
// (width, height, perceptual_hash). The storage core opaquely passes
// it to OnCommitted.
type Metadata interface {
    ProductName() string
}
```

## Format conversion (nova-image)

`nova-image`'s `AnalyzeUpload` MAY transform incoming PNG/BMP/TIFF
uploads into WebP before encryption. The motivation is the
"spectral-bloat" use case: high-resolution screenshots and
spectrograms commonly arrive as 30+ MB PNGs but lossless WebP
encoding reduces them to 5-7 MB without visual loss.

Behaviour:

- Configured via `nova-image.format_conversion` in operator config.
  Default off in Phase 1; recommend on for deployments expecting
  large screenshot/spectral uploads.
- When on, `AnalyzeUpload` re-encodes the plaintext to WebP
  (lossless for screenshots, configurable quality for photos)
  before returning it to the storage core for encryption.
- The transformation happens before the envelope CID is computed,
  so the stored CID is the CID of the transformed bytes. The
  original is not retained.
- Per-collection override: `collection.policy.preserve_original_format=true`
  disables conversion for collections that need byte-perfect retention
  (e.g., scientific imaging archives).

## Registration

Products are registered at coordinator construction time:

```go
// cmd/coordinator/main.go
coord, err := coordinator.New(coordinator.Config{
    DataDir: cfg.DataDir,
    DB:      cfg.DB,
    // ...
})
if err != nil { ... }

// Register products. Order matters for migrations only.
coord.RegisterProduct(image.New(image.Config{
    FormatConversion: cfg.Image.FormatConversion,
    PerceptualHash: cfg.Image.PerceptualHash,
    // ...
}))
// future: coord.RegisterProduct(video.New(...))

if err := coord.Run(ctx); err != nil { ... }
```

`coord.Run` runs all migrations, mounts all routes, then starts the
HTTP server, federation endpoint, and orchestrator.

The set of registered products is **compile-time** in Phase 1.
Operators choose their build by editing `cmd/coordinator/main.go`
or by using a build-tagged variant. Phase 6+ may explore Go-plugin
or WASM-based runtime loading; that complexity is not justified now.

## URL prefix conventions

| Prefix       | Owned by         | Notes |
|--------------|------------------|---|
| `/blob/...`  | storage core     | Generic content addressing. |
| `/api/v1/...`| storage core     | Management API. |
| `/fed/v1/...`| storage core     | Federation (Nebula-only). |
| `/health`, `/legal/...` | storage core | Public meta-endpoints. |
| `/i/...`     | `nova-image`     | Image content + transforms. |
| `/v/...`     | future `nova-video` (reserved) | |
| `/a/...`     | future `nova-audio` (reserved) | |
| `/d/...`     | future `nova-document` (reserved) | |
| `/r/...`     | future `nova-archive` (reserved; "raw archive") | |

Products MUST NOT mount routes under any storage-core prefix. The
coordinator's chi router enforces this with a registration-time
check.

## Database conventions

The storage core owns the canonical tables defined in
`DATA_MODEL.sql`. A product layer that needs persistent side-table
metadata follows these conventions:

1. **Naming.** Side tables are named `{product_name}_metadata` for
   the canonical metadata row, or `{product_name}_{topic}` for
   ancillary tables. Examples: `image_metadata`, future
   `video_segments`.
2. **Primary key = `cid`.** Side tables join to `blobs(cid)`. They
   MUST declare `ON DELETE CASCADE` from `blobs` so the storage
   core's tombstone flow cleans them up automatically.
3. **No foreign keys back into another product's tables.**
   Cross-product joins go through `blobs.product` and `blobs.cid`.
4. **Migrations live with the product.** Each product ships its own
   `migrations/` directory containing forward-only Postgres SQL
   files (`NNN_description.sql`). The coordinator runs them in
   numeric order at boot. **Exception: `image_metadata` is
   core-owned** — it is defined in `docs/specs/DATA_MODEL.sql` and
   included in `internal/db/migrations/0001_init.sql` (the storage
   core's initial migration). The products-own-migrations rule
   governs future new product tables. Accordingly, `nova-image`
   ships no migrations of its own in Phase 1 (`Migrations()` returns
   an empty FS).
5. **No shared enums across products.** If two products need a
   "format" enum they each define their own. Sharing causes coupled
   migrations and breaks the modular contract.

## Derivatives (v2 — first-class blobs)

In v1 derivatives lived in a separate `derivatives` table. In v2 they
are full first-class `blobs` rows with `parent_cid`,
`derivative_preset`, and `derivative_format` columns. This means:

- Every derivative has its own `data_encryption_keys` row.
- Every derivative has its own `state` (lifecycle independent of
  the parent's, but cascaded on parent state changes).
- Every derivative has its own `pin_assignments` rows (replicated
  across the federation per its content class — `normal` for image
  derivatives, default `R=3`).
- Tombstoning a parent cascades to tombstone all derivatives. The
  product's `OnDelete` hook is responsible for this cascade.
- Quarantining or legal-holding a parent cascades to derivatives
  the same way.

The lookup `(parent_cid, preset, format) → derivative_cid` is served
by the unique partial index `blobs_derivative_lookup_idx` on
`blobs (parent_cid, derivative_preset, derivative_format) WHERE parent_cid IS NOT NULL`.

When `nova-image` generates a derivative on cache miss:

1. Read parent envelope from local Kubo, decrypt.
2. Apply transform (resize, format-convert, etc.) to plaintext.
3. Encrypt result with a fresh per-blob key under
   `data_encryption_keys`.
4. Import envelope into local Kubo, obtain derivative CID.
5. INSERT `blobs` row with `parent_cid`, `derivative_preset`,
   `derivative_format`, `product = 'image'`, fresh
   `encryption_key_id`.
6. INSERT `image_metadata` row (width/height; perceptual_hash NULL,
   parent's hash is canonical).
7. Commit transaction.
8. Pin locally and enqueue replication (default `R=3` for `normal`
   class).
9. Stream the derivative bytes to the original requester (with the
   correct `Cache-Control` per the openapi.yaml stratification).

## Read pipeline

Read paths are entirely owned by the product's `RegisterRoutes`.
The product handler:

1. Resolves the CID and (optional) preset/format from the URL.
2. Looks up the cached derivative if the URL implies a transform
   (e.g., `/i/{cid}/w512.webp`). The lookup is a partial-index scan
   on `blobs (parent_cid, derivative_preset, derivative_format)`.
3. On cache miss: fetches the parent CID from local Kubo, decrypts
   via the storage core, transforms (e.g., govips), creates the
   derivative row per the procedure above.
4. Sets `Cache-Control` per the stratified policy in `openapi.yaml`
   (immutable for public; private/no-store for signed/quarantined/
   tombstoned).
5. Streams the bytes to the response writer.

The storage core provides helpers (`storage.Get`, `storage.Decrypt`,
`storage.PutDerivative`) so products do not re-implement this.

## Versioning compatibility

The interface above is `pkg/coordinator/product@v1`. Backwards-
compatible additions:

- New optional methods via wrapper interfaces (e.g.,
  `SearchProvider`, `ManifestExporter`).
- New fields on existing struct types, with zero-value default
  meaning "not provided."
- New URL prefixes for new products.

Backwards-incompatible changes (renaming methods, removing fields,
changing semantics) require a major version bump and a deprecation
period during which v1 and v2 are both supported.

## Reference: nova-image

`nova-image` is the canonical implementation. Its module layout
under the monorepo:

```
internal/transform/         govips wrapper, transform pipeline
internal/perceptualhash/    Phase 3: Go-native 256-bit pHash (near-dup dedup) + BK-tree
internal/imageapi/          /i/* route handlers
internal/imagemoderation/   Phase 4: PDQ scanner vs StopNCII/NCMEC (external blocklist matching)
nova-image/migrations/      (none in Phase 1; image_metadata is core-owned in 0001_init.sql)
web/widget/                 Drag-and-drop uploader
```

`AnalyzeUpload` for `nova-image`:
1. Verify MIME is one of `image/jpeg`, `image/png`, `image/webp`,
   `image/avif`, `image/tiff`, `image/bmp`, `image/gif`.
2. Decode to raw pixels, extract width/height.
3. **Phase 1 (pass-through):** skip perceptual hash computation;
   `ImageModeration.Scan` is a no-op allow. **Phase 3** adds a
   Go-native 256-bit pHash (goimagehash `ExtPerceptionHash`) for
   near-duplicate dedup. **Phase 4** adds PDQ computation and
   `ImageModeration.Scan` against the StopNCII/NCMEC blocklist —
   PDQ blocklist match → `ScanResult{Action: 'tombstone',
   Rule: 'pdq_match'}` (severe content) or `'quarantine'` (operator
   blocklist).
4. If `format_conversion` enabled, re-encode to WebP (lossless for
   screenshots/spectrograms; configurable quality otherwise).
5. Return `ImageMetadata{width, height, perceptual_hash}` and the
   (possibly transformed) plaintext. `perceptual_hash` is NULL in
   Phase 1; populated from Phase 3 onward.

`OnCommitted` for `nova-image`:
1. Pre-warm the most common derivative presets (`thumb`, `og`)
   asynchronously, so the first user-visible read is fast.
2. Emit `image.created` webhook.

Phase 1 ships `nova-image`. The interface above is the contract it
honors and the contract any future product must honor.

## Cross-references

- Schema: `docs/specs/DATA_MODEL.sql` (`blobs`, `image_metadata`)
- Encryption: `docs/specs/ENCRYPTION_ENVELOPE.md`
- Deterministic CIDs: `docs/specs/IPFS_IMPORT_RULES.md`
- Moderation: `docs/legal/DMCA_PROCEDURE.md`,
  `docs/legal/SEVERE_CONTENT_PROCEDURE.md`
