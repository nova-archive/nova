# Product Module Interface

Status: **Phase 0 — normative.** Defines how content-type-specific
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

The interface is part of `pkg/coordinator`'s public API and follows
its semver discipline. Adding methods is a major-version change.
Adding non-required hooks via wrapper interfaces is a minor-version
change.

## Go interface

```go
// Package product, part of pkg/coordinator/product.
package product

import (
    "context"
    "io"

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

    // OnUpload is called after the storage core has computed the
    // CID, encrypted the bytes, and persisted the blobs row. The
    // product MUST NOT mutate the blob; it MAY persist side-table
    // metadata and run moderation. Returning an error fails the
    // upload and the storage core rolls back.
    //
    // plaintext is provided for the lifetime of this call only;
    // the product MUST NOT retain it.
    OnUpload(ctx context.Context, blob *storage.Blob, plaintext io.Reader) error

    // OnDelete is called as part of the tombstone flow, before the
    // crypto-shred and unpin broadcast. The product cleans up
    // side-table rows. Errors are logged and metric-counted but
    // do not block the deletion (the storage core's audit and
    // shred steps are authoritative).
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

// Optional interface: products that ship a moderation scanner
// implement this. The coordinator's moderation pipeline iterates
// every registered scanner; an action of "tombstone" or
// "quarantine" from any scanner short-circuits.
type ModerationScanner interface {
    Scan(ctx context.Context, blob *storage.Blob, plaintext io.Reader) (*ScanResult, error)
}

type ScanResult struct {
    Action   storage.ModerationAction // allow | quarantine | tombstone
    Rule     string                   // matches moderation_decisions.rule_ref
    Distance int                      // optional similarity distance
    Notes    string
}
```

The full Go signatures (including parameter docs) live in
`pkg/coordinator/product/product.go` once Phase 1 begins.

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
coord.RegisterProduct(image.New(image.Config{...}))
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

| Prefix       | Owned by         | Notes                                 |
|--------------|------------------|---------------------------------------|
| `/blob/...`  | storage core     | Generic content addressing.           |
| `/api/v1/...`| storage core     | Management API.                       |
| `/fed/v1/...`| storage core     | Federation (Nebula-only).             |
| `/health`, `/legal/...` | storage core | Public meta-endpoints.        |
| `/i/...`     | `nova-image`     | Image content + transforms.           |
| `/v/...`     | future `nova-video` (reserved) |                          |
| `/a/...`     | future `nova-audio` (reserved) |                          |
| `/d/...`     | future `nova-document` (reserved) |                       |
| `/r/...`     | future `nova-archive` (reserved; "raw archive") |        |

Products MUST NOT mount routes under any storage-core prefix. The
coordinator's chi router enforces this with a registration-time check.

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
   numeric order at boot.
5. **No shared enums across products.** If two products need a
   "format" enum they each define their own. Sharing causes coupled
   migrations and breaks the modular contract.

## Upload pipeline

The storage core's upload handler executes this sequence per request:

1. Stream incoming bytes to a temp area (tus chunks accumulate, or
   multipart parts buffer).
2. On finalize: compute the plaintext hash; route to the chosen
   product based on declared MIME type or `Upload-Metadata.product`.
3. Call `product.OnUpload(ctx, blob, plaintext)` **before** the
   transaction commits. If it returns an error, abort the entire
   upload (no `blobs` row, no envelope written, no IPFS pin).
4. Call any `ModerationScanner.Scan(...)` registered by any product
   (cross-product moderation is fine; an image's PDQ scanner sees
   image uploads, etc.). Any `tombstone` or `quarantine` result
   short-circuits the upload and records a `moderation_decisions`
   row.
5. Encrypt the plaintext per `ENCRYPTION_ENVELOPE.md`, compute the
   envelope CID, write to local Kubo, insert the `blobs` row, and
   enqueue replication.

`OnUpload` is the place to write the side-table row (e.g., for
images: decode dimensions and PDQ hash, insert `image_metadata`).

## Read pipeline

Read paths are entirely owned by the product's `RegisterRoutes`.
The product handler:

1. Resolves the CID and (optional) preset/format from the URL.
2. Looks up the cached derivative if the URL implies a transform
   (e.g., `/i/{cid}/w512.webp`). Derivative CIDs are tracked in
   the storage core's `derivatives` table.
3. On cache miss: fetches the original CID from local Kubo,
   decrypts via the storage core, transforms (e.g., govips), inserts
   the derivative row, encrypts the derivative, pins it locally,
   enqueues replication.
4. Sets the `Cache-Control: public, max-age=31536000, immutable`
   header.
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

`nova-image` is the canonical implementation. Its module layout under
the monorepo:

```
internal/transform/         govips wrapper, transform pipeline
internal/perceptualhash/    PDQ + BK-tree
internal/imageapi/          /i/* route handlers
internal/imagemoderation/   ModerationScanner that scans against StopNCII
nova-image/migrations/      Side-table migrations (image_metadata)
web/widget/                 Drag-and-drop uploader
```

Phase 1 ships `nova-image`. The interface above is the contract it
honors and the contract any future product must honor.
