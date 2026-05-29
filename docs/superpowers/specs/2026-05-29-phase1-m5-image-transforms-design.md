# Phase 1 M5 — Image Transforms (nova-image Product Layer) Design

**Goal:** Turn `nova-image` into a real product. A curl-driven `GET /i/{cid}/w512.webp`
against an uploaded JPEG produces a 512-px WebP, persisted as a first-class derivative blob
with its own key and served from cache on the next request; common presets (`thumb`, `og`)
are pre-warmed asynchronously the moment a parent image commits. M5 also lands the generic
`pkg/coordinator/product.Product` interface + `RegisterProduct` wiring that every future
product layer plugs into.

**Architecture (one line):** A new `nova-image` package implements the (v0-unstable)
`product.Product` interface; the storage core's `Service.Put` gains an injected,
product-agnostic **write hook** (so `storage` never imports `product` — no import cycle)
that runs image decode/metadata/optional format-conversion before encryption; a new
`Service.PutDerivative` mirrors `Put` for derivative blobs; nova-image's `/i/*` handlers
do **single-flight find-or-create** of derivatives via a bounded govips transform pipeline;
and `coordinator.Run` finally starts the (M2) worker pool to run the `derivative_prewarm`
handler that `OnCommitted` enqueues.

**Status:** approved design (review incorporated — Denial-of-Storage, CGO memory, auth-join,
deletion-cascade corrections folded in). Implementation plan follows in
`docs/superpowers/plans/2026-05-29-phase1-m5-image-transforms.md`.

**Author:** Bug Plowman (operator), Claude (implementation partner).

---

## Purpose and scope

M5 is the fifth Phase 1 milestone and the last of the walking skeleton (M1–M5). M4 built the
**product-agnostic** write path (`Service.Put`: validate → encrypt → import → commit, with
unpin-on-rollback) and left four clean seams for M5, each confirmed in the committed code:

- `pkg/coordinator/product/` is empty — the `Product` interface does not exist yet.
- `Service.Put` runs a generic MIME floor and sets `product='raw'`; the `AnalyzeUpload`
  hook is a documented no-op seam.
- `InsertBlob` carries no `parent_cid`/`derivative_preset`/`derivative_format` — there is
  no derivative write path.
- `internal/jobs/kinds/derivative_prewarm.go` is a no-op stub and the worker pool
  (present since M2) is **never `Run()`**.

M5 fills all four. It is the milestone that introduces the project's **first cgo dependency**
(libvips, via govips) and therefore settles the M3-deferred runtime base-image decision.

### In scope

- **`pkg/coordinator/product`** (v0-unstable): the `Product` interface (`Name`,
  `AcceptedMimeTypes`, `AnalyzeUpload`, `OnCommitted`, `OnDelete`, `RegisterRoutes`,
  `Migrations`), `Metadata`, `UploadContext`; plus `storage.ScanResult` (Phase-1
  pass-through, always `allow`).
- **`coordinator.RegisterProduct`**: registers a product, enforces the reserved-prefix rule,
  mounts `RegisterRoutes`, runs `Migrations` (empty for nova-image in P1 — see reconciliation
  #1). nova-image is registered in `cmd/coordinator`.
- **Storage write hook**: `Service.Put` gains an injected `storage.WriteHook` seam (defined
  in `storage`, satisfied by a coordinator-side nova-image adapter) called after the MIME
  floor and before encryption — returning a `ScanResult`, an optional transformed plaintext
  (+ its MIME), and a transactional side-table `Persist` closure.
- **`Service.PutDerivative`**: the derivative write transaction (fresh key → deterministic
  import → commit `blobs`+`blob_manifests`+`blob_blocks`+`data_encryption_keys`+
  `image_metadata`), with unpin-on-rollback and **derivative-lookup-unique-violation
  recovery** (the cross-process backstop for the random-nonce dedup nuance below).
- **`nova-image`**: the govips transform wrapper (decode, resize, encode, format map,
  megapixel guard); `/i/*` read handlers with single-flight find-or-create; the
  `image_metadata` read/write queries; a Phase-1 pass-through moderation scanner; `OnCommitted`
  (prewarm enqueue) and `OnDelete` (derivative cascade).
- **`/api/v1/images`** (core-mounted; forces `product='image'`, early 415 on non-image MIME).
- **Formats**: JPEG, PNG, WebP, GIF (static), TIFF, BMP as inputs; JPEG/PNG/WebP default
  outputs; **AVIF and JXL supported but operator-gated, OFF by default as outputs**; optional
  upload-time **PNG/BMP/TIFF → WebP format-conversion** (config-gated, default off, honoring
  `collection.policy.preserve_original_format`).
- **Worker pool startup** in `coordinator.Run` + the real `derivative_prewarm` handler.
- **Operator config** (`image:` section): allowed input/output formats, dimension whitelist,
  presets, format-conversion, and the resource bounds below.
- **Resource bounds & abuse resistance**: dimension whitelist (Denial-of-Storage),
  max-megapixels (decompression bombs), transform-concurrency semaphore, libvips cache cap
  (cgo C-heap), single-flight + detached-context generation.
- **Read-path auth change**: a single parent-aware visibility query (no N+1 / sequential
  derivative→parent chain).
- Unit + nginx-fronted integration tests; CI installs a codec-complete libvips.

### Out of scope (with the milestone that owns each)

- **Perceptual hashing** — `image_metadata.perceptual_hash` is written **NULL** in M5. The
  two-track strategy (Go-native pHash for internal dedup → **Phase 3**; real PDQ for external
  StopNCII/NCMEC matching → **Phase 4**) is documented but not built (reconciliation #2).
- **The moderation/tombstone invocation** (quarantine, scheduled tombstone, crypto-shred,
  legal-hold) — **M9**. M5 ships the `OnDelete` hook and its semantics so M9 can rely on
  them; M5 does not wire the caller.
- Bearer auth + real `owner_id` — **M6** (M5 runs under the M3 `nova_dev` anonymous floor).
- Collection-management API — **M6** (M5 tests seed collections directly, as M3/M4 did).
- Authenticated metadata read/update (`GET/PATCH /api/v1/blobs|images/{cid}`) — **M6**.
- Integrity audits incl. `derivative_state_consistent` — **M8** (M5 produces the derivative
  rows it will check; the parent-aware visibility query is shaped to keep that audit O(1)).
- Animated GIF / multi-frame handling — deferred (M5 serves GIF originals byte-for-byte and
  transforms the first frame only; full animation is a later milestone).
- The production `docker/Dockerfile` + compose coordinator service — **M13** (M5 needs a dev
  build path + CI libvips only; it mandates the base-image *decision*, not the prod image).
- A Kubo orphan-pin reconciliation sweep — out of Phase 1 (M4 residual risk, unchanged).
- `oapi-codegen` adoption — still deferred (the `/i/*` surface is path-param + bytes, cheaper
  hand-written; revisit when the `/api/v1` JSON surface grows in M6).

---

## Source of truth and required doc reconciliations

This spec is authoritative for M5 behavior. Drifts surfaced during design are resolved here;
edits are applied after this spec is reviewed (the same pattern M3/M4 used).

1. **`image_metadata` is core-owned, not a product migration.** `PRODUCT_MODULE_INTERFACE.md`
   says "migrations live with the product" (`nova-image/migrations/0001_image_metadata.sql`),
   but `image_metadata` + the `blobs` derivative columns + `blobs_derivative_lookup_idx`
   already exist in `0001_init.sql` (it is `DATA_MODEL.sql` v2 verbatim) and are referenced by
   the core M8 `derivative_state_consistent` audit. **Resolution: `image_metadata` stays
   core-owned; nova-image's `Migrations()` returns an empty FS in Phase 1.** The
   "products own their side-table migrations" contract governs *future new* tables a product
   introduces, not the canonical schema. **Edit:** add a note to `PRODUCT_MODULE_INTERFACE.md`
   § "Database conventions" and to the M5 milestone line. **M5 ships no new migration.**

2. **PDQ → two-track, NULL in M5.** Specs say "compute PDQ" uniformly, but Phase 1 has no
   blocklist scan (manual moderation), so a hash would be stored unused; and there is no
   vetted Go PDQ library (`goimagehash` is aHash/dHash/pHash, BSD-2-Clause; PDQ reference is
   C++/Python in ThreatExchange). A non-PDQ placeholder is worse than NULL — algorithm-
   incompatible with the PDQ hash sets StopNCII/NCMEC distribute, forcing a re-decode anyway.
   **Resolution:**
   - **Phase 3 (dedup):** Go-native 256-bit DCT pHash (`goimagehash.ExtPerceptionHash(16,16)`
     → 32 bytes, fits the existing `bytea` column) for *internal* near-duplicate clustering,
     surfaced **advisory** in the admin UI (never auto-delete; sovereignty + false-positive
     risk). Queue-driven backfill over the operator's own corpus.
   - **Phase 4 (severe content):** **real PDQ** (FFI to the ThreatExchange reference, validated
     against official PDQ vectors) for *external* known-bad matching (StopNCII/NCMEC) →
     synchronous reject / quarantine+legal-hold. Lives where the severe-content workflow
     already is.
   - **M5:** `image_metadata.perceptual_hash = NULL` for all originals and derivatives.

   **Edits:** `ROADMAP.md` (name the pHash in Phase 3, PDQ in Phase 4),
   `PRODUCT_MODULE_INTERFACE.md` (split the monolithic "PDQ" in `AnalyzeUpload` / "Reference:
   nova-image" into the two tracks), and the `perceptual_hash` comment in `DATA_MODEL.sql` +
   `0001_init.sql` (comment-only; no schema change).

3. **`/api/v1/images` is M5** (already reconciled out of M4). M5 implements it — core-mounted,
   `product='image'`, early 415 on non-image MIME, 422 wired but inert in Phase 1.

4. **JXL is absent from the OpenAPI `ext` enums.** `/i/{cid}.{ext}` enumerates
   `[jpg,jpeg,png,webp,avif,gif]` and the resize routes `[jpg,jpeg,png,webp,avif]` — no `jxl`.
   **Edit:** add `jxl` to those enums, with a description noting it is operator-gated and
   off by default (browser delivery support was ~12–17% in early 2026; Chrome default
   enablement expected H2 2026, Firefox 152 in June 2026).

5. **Dynamic dimensions are operator-whitelisted.** The OpenAPI `{w}/{h}` params declare
   `minimum:1, maximum:8192` with no whitelist. To close the Denial-of-Storage hole (below),
   the operator's `allowed_widths`/`allowed_boxes` **further constrain** these; an in-range
   but non-whitelisted dimension returns **400** (no transform, no persist). **Edit:** note
   this on the `/i/{cid}/{w}x{h}.{ext}` and `/i/{cid}/w{w}.{ext}` parameter descriptions.

6. **Runtime base image = Debian-slim/glibc with a codec-complete libvips** (resolves the
   M3-deferred "alpine vs distroless" decision; rationale in "Resource bounds"). **Edit:** the
   Phase-1 design's open question #1 and "What requires your decision … container base image"
   are resolved to Debian-slim. **Worker-pool startup** moves from "M5/M8" to **M5**.

The openapi upload/image endpoints declare `bearerAuth`; auth issuance is M6. Until then M5
runs under the M3 `nova_dev` anonymous floor (a production build still refuses
`auth.anonymous: true`). No openapi auth edit is needed — same posture as M3/M4.

---

## Preconditions from M2 / M3 / M4

M5 builds directly on these committed symbols. If any diverges, M5 estimates are optimistic.

| Dependency | Symbol / location | M5 use |
|---|---|---|
| Write transaction | `(*storage.Service).Put`, `commit`, assembly semaphore | Extended with the `WriteHook` seam; `PutDerivative` mirrors `commit` |
| Write context/result | `storage.PutContext`, `PutResult` | `+ DerivativeContext`; `PutResult` reused |
| Read/auth | `(*storage.Service).Resolve` / `OpenBytes`; `GetBlobCore`; `ResolveBlobVisibility`; `GetDEKByBlob` | Parent auth + derivative serving; visibility query made parent-aware |
| Envelope + keystore | `envelope.V1().Encrypt/Decode`, `Keystore.Wrap/Unwrap` | Encrypt derivatives; decrypt parents to transform |
| Deterministic import | `ipfs.Backend.AddDeterministic` / `Get` / `Unpin` | Import derivative envelopes; fetch parent envelope; unpin on rollback |
| Job queue + pool | `jobs.Queue.Enqueue`; `(*WorkerPool).RegisterHandler` / `Run` | Prewarm enqueue + the pool finally runs in `coordinator.Run` |
| Prewarm stub | `kinds.KindDerivativePrewarm` (+ stub) | Stub body replaced with the real handler |
| Schema (already present) | `blobs.parent_cid/derivative_preset/derivative_format`, `blobs_derivative_lookup_idx`, `image_metadata` (0001_init) | Derivative writes + lookup; **no new migration** |
| Write queries | `internal/db/queries/writes.sql` (`InsertBlob` has no derivative cols) | `+ InsertDerivativeBlob`, `+ GetDerivativeCID`, parent-aware visibility |
| Upload handlers | `internal/api/handlers/upload.go` (multipart sets `product`) | `+ /api/v1/images`; `UploadResult.urls.presets` populated for images |
| Dev nginx harness | `docker/nginx/nova.dev.conf` | Extended to pass `/i/*` + `/api/v1/images` |
| `nova_dev` floor | `internal/auth/anonymous_{dev,prod}.go` | Anonymous reads/writes in dev; refuse-to-start in prod |

---

## Architecture

```
cmd/coordinator/main.go        ── coord.RegisterProduct(novaimage.New(cfg.Image)); start worker pool
        │
internal/api/server.go         ── mounts /api/v1/images (CORE — reserved prefix); product RegisterRoutes mounts /i/*
        │
pkg/coordinator/coordinator.go ── RegisterProduct (prefix check, RegisterRoutes, Migrations);
        │                          adapts product.Product → storage.WriteHook (closes over jobs.Queue);
        │                          Run() starts jobs.WorkerPool with the derivative_prewarm handler
        │
nova-image/                    ── implements pkg/coordinator/product.Product
   product.go                    Name/AcceptedMimeTypes/AnalyzeUpload/OnCommitted/OnDelete/RegisterRoutes/Migrations
   internal/transform/           govips wrapper: decode, resize (wN aspect, WxH cover), encode, format map,
                                  megapixel guard; vips.Startup cache caps; transform-concurrency semaphore
   internal/imageapi/            /i/* handlers; single-flight find-or-create; parse → validate → resolve → serve
   internal/imagemeta/           image_metadata read/write (width/height/alt/caption; perceptual_hash NULL)
   internal/imagemoderation/     Phase-1 pass-through scanner (Action: allow) — PDQ/blocklist is Phase 3/4
        │ (storage never imports product; the seam below inverts the dependency)
        ▼
pkg/coordinator/storage/
   put.go         Service.Put: + WriteHook call (Analyze → transform → ScanResult → Persist; OnCommitted)
   derivative.go  Service.PutDerivative (NEW): encrypt → import → commit derivative + image_metadata
   blob.go        Service.Resolve: parent-aware single-query visibility; transform helpers
   types.go       + DerivativeContext; WriteHook / AnalyzeResult seam types (storage-local)
   errors.go      + ErrModerationRejected, ErrFormatNotAllowed, ErrDimensionNotAllowed, ErrImageDecode
        │ uses
        ▼
internal/envelope · internal/ipfs · internal/db/gen · internal/jobs
pkg/coordinator/product/         ── Product, Metadata, UploadContext (v0-unstable); storage.ScanResult
```

### The product seam (why no import cycle)

`pkg/coordinator/product.Product` references `storage` types (`ScanResult`, `Blob`), so
`product` imports `storage`. Therefore **`storage` must not import `product`.** The seam that
`Service.Put` calls is defined *in `storage`* with storage-local types only, and the
coordinator supplies a nova-image adapter:

```go
// pkg/coordinator/storage — the seam Put calls. No product import.
type WriteHook interface {
    // Analyze runs after the MIME floor, before encryption. plaintext is valid
    // for the call only; the hook MUST NOT retain it.
    Analyze(ctx context.Context, pc PutContext, plaintext []byte) (AnalyzeResult, error)
    // OnCommitted runs best-effort after a successful commit (errors logged, not fatal).
    OnCommitted(ctx context.Context, ref CommittedRef)
}

type AnalyzeResult struct {
    Scan        ScanResult                                  // Action != "allow" ⇒ reject (422)
    Transformed []byte                                      // nil ⇒ store the original bytes
    ResultMIME  string                                      // set iff Transformed != nil (e.g. image/webp)
    Persist     func(ctx context.Context, tx pgx.Tx, cid string) error // side-table write inside Put's tx; nil ⇒ none
}
```

nova-image implements the canonical `product.Product` (the spec contract, Phase-4-facing); a
small `coordinator` adapter bridges `product.Product → storage.WriteHook` and closes over the
`jobs.Queue` (so the queue dependency lives in coordinator/nova-image, never in the storage
core). This keeps `storage` product-agnostic exactly as M4 left it.

### `PutDerivative` signature

```go
// DerivativeContext carries validated derivative-write metadata.
type DerivativeContext struct {
    ParentCID string // REFERENCES blobs(cid); authorizes + cascades
    Preset    string // canonical key part: 'thumb' | 'w512' | '512x384'
    Format    string // 'webp' | 'jpeg' | 'png' | 'avif' | 'jxl'
    MIME      string // image/webp, ...
    Width     int
    Height    int
}

// PutDerivative encrypts a derivative under a fresh per-blob key, imports it
// deterministically, and commits the derivative blob + manifest + blocks + DEK +
// image_metadata in one transaction. Idempotent under the (parent,preset,format)
// unique index: a loser unpins its orphan import and returns the winner's CID.
func (s *Service) PutDerivative(ctx context.Context, plaintext []byte, dc DerivativeContext) (*PutResult, error)
```

---

## Write-path integration (the `AnalyzeUpload` seam)

`Service.Put` changes minimally. After the existing MIME floor (step 3) and before
encryption, and inside the existing commit transaction:

```
3.5  if hook != nil:
        ar := hook.Analyze(ctx, pc, buf)
        ar.Scan.Action != "allow"  ⇒ return ErrModerationRejected (422)   # inert in Phase 1
        if ar.Transformed != nil:  buf, mime = ar.Transformed, ar.ResultMIME   # format-conversion path
                                   # re-check len(buf) ≤ max_upload_size (re-encode is bounded)
5    encrypt buf (possibly transformed) as today
7    commit tx … after InsertBlob/Manifest/Blocks/CollectionItem:
        if ar.Persist != nil: ar.Persist(ctx, tx, cid)        # writes image_metadata atomically
9.   post-commit (best-effort): if hook != nil: hook.OnCommitted(ctx, CommittedRef{CID, Product, Visibility…})
```

nova-image's `Analyze` (Phase 1):

1. Verify MIME ∈ `AcceptedMimeTypes` (the `/api/v1/images` early-415 check happens at the
   handler; here it is the authoritative gate). Non-image ⇒ a product-reject error → 415.
2. Decode header for `width`/`height` (govips), **subject to `max_megapixels`** (decode-bomb
   guard) — reject oversize as `ErrImageDecode` (422 `image_decode_failed`).
3. Scanner: pass-through, `ScanResult{Action: "allow"}`. (PDQ/blocklist is Phase 3/4.)
4. If `format_conversion.enabled` and the input is PNG/BMP/TIFF and the blob's collection does
   **not** set `preserve_original_format`: re-encode to the target (WebP, lossless default) →
   `Transformed` + `ResultMIME`.
5. Return `Persist` = the `image_metadata` insert (`cid, width, height, perceptual_hash=NULL,
   alt_text, caption`).

`OnCommitted` enqueues `derivative_prewarm{cid, prewarm_presets}` (and, post-M5, the
`image.created` webhook).

---

## Read / transform path (`/i/*`)

nova-image's `RegisterRoutes` mounts (the OpenAPI surface):
`/i/{cid}`, `/i/{cid}.{ext}`, `/i/{cid}/{w}x{h}.{ext}`, `/i/{cid}/w{w}.{ext}`,
`/i/{cid}/p/{preset}.{ext}`, `/i/{cid}.json`.

Handler algorithm:

```
1. Parse cid + transform spec (none | ext | wN | WxH | preset) + ext.
2. Validate ext ∈ image.allowed_output_formats           else 406 format_not_allowed
   Validate dimensions:
       preset            → must be a configured preset    else 404
       wN  / WxH         → must be in allowed_widths / allowed_boxes  else 400 dimension_not_allowed
       (empty whitelist ⇒ dynamic routes always 400 ⇒ presets-only posture)
3. Resolve the PARENT (cid is the parent CID, from the URL):
       storage.Resolve(cid) → BlobView   # state + parent-aware visibility (single query)
       not found/quarantined/gone        → 404 / 451 / 410 (reuse M3 mapping)
       private                           → 401 (signed_url_required)
       product != 'image'                → 415
4. Original request (no transform, ext absent or == stored format):
       stream OpenBytes(parent); image headers (X-Nova-Width/Height from image_metadata)
5. Transform request:
       key := canonical(preset | "wN" | "WxH", format)
       deriv, found := GetDerivativeCID(parent, key.preset, key.format)
       found & active → serve the derivative (its own DEK; same OpenBytes path)
       miss → singleflight(key, detached-ctx):
                 plaintext := OpenBytes(parent)                       # decrypt parent
                 out, w, h := transform.Render(plaintext, spec, format)  # govips, bounded
                 res := PutDerivative(out, {parent, key.preset, key.format, mime, w, h})
              stream out (the generating request); concurrent waiters read the winner
6. Cache-Control: parent public → "public, max-age=31536000, immutable"; else
   "private, max-age=300, must-revalidate" (reuse M3 stratification, keyed on PARENT visibility).
```

**Derivative dedup nuance (random-nonce).** Like M4 originals, a freshly-encrypted derivative
has a random nonce, so regenerating the *same* `(parent, preset, format)` yields a *different*
CID. CID-level dedup is therefore impossible; the **`blobs_derivative_lookup_idx` unique index
on `(parent_cid, derivative_preset, derivative_format)` is the dedup key.** `PutDerivative`
inserts with `ON CONFLICT` on that index `DO NOTHING`; if it affected 0 rows (a concurrent or
cross-process writer won), it **unpins its orphan import** and returns the winner's CID via
`GetDerivativeCID`. In-process this is rare (single-flight collapses it); cross-process (Phase
2) it is the correctness backstop.

**Single-flight + CGO reality.** Generation is keyed by `(parent, preset, format)` in an
in-process `singleflight`-style group (the same model as the tus PATCH lock). Because libvips
runs in cgo and **ignores Go `context` cancellation** — a disconnected client cannot interrupt
an in-flight encode — generation runs under a **detached context with a transform timeout**, so
a client disconnect neither aborts the commit nor wastes the completed transform (the work is
deliberately not thrown away). The real protections against a stuck/oversized transform are the
**hard bounds** (max-megapixels, dimension whitelist, transform-concurrency semaphore), not
cancellation.

---

## `/api/v1/images`

Mounted by the **core** (it lives under the reserved `/api/v1` prefix products may not touch).
It is `POST /api/v1/blobs` with `product` forced to `image`, and the registered image product's
`AcceptedMimeTypes` enforced **before** the body is buffered (declared/sniffed MIME not an
accepted image type → **415** early). Otherwise it shares the `Put` + `WriteHook` path. The
**422** moderation reject is wired (mapped from `ErrModerationRejected`) but never fires in
Phase 1 (pass-through scanner). `UploadResult.urls.presets` is populated for image blobs with
the configured `prewarm_presets` URLs.

---

## Prewarm, `OnCommitted`, and the worker pool

`coordinator.Run` now constructs and runs the worker pool (it has existed since M2 but was
never started):

```
pool := jobs.NewWorkerPool(queue, jobs.WorkerOptions{ /* defaults: 4 workers */ })
pool.RegisterHandler(kinds.KindDerivativePrewarm, derivativePrewarmHandler)   // real body
go pool.Run(ctx)                                                              // stops on ctx cancel
```

`derivativePrewarmHandler(payload {parent_cid, presets[]})`: for each preset, run the same
single-flight find-or-create the read path uses (so a concurrent read and the prewarm collapse
to one transform). Idempotent via the unique index. **Best-effort** per the `OnCommitted`
contract: a failed transform is logged + metric-counted and the job retries/backs off, but it
**never rolls back the parent upload**. Prewarm transforms consume the **same** transform-
concurrency semaphore and libvips cache as read-path misses (one global bound — see below).

---

## `OnDelete` cascade (hook + semantics ship in M5; invocation is M9)

`PRODUCT_MODULE_INTERFACE.md` makes the product's `OnDelete` responsible for the parent →
derivative cascade. Leaving it implicit means a tombstoned/quarantined parent keeps serving
derivative thumbnails — a real legal-liability hole. M5 defines and implements it; M9 calls it
from the quarantine/tombstone flow.

`nova-image.OnDelete(ctx, blob)` runs *before* the core crypto-shred + unpin broadcast and:

1. **Bulk state cascade:** `UPDATE blobs SET state = $newState WHERE parent_cid = $cid` — every
   derivative moves to the parent's new state (`quarantined` / `soft_deleted` / `tombstoned`).
2. **Shred-set enumeration:** every derivative has its **own** `data_encryption_keys` row;
   `OnDelete` guarantees the core crypto-shred covers all of them (enumerated via
   `parent_cid = $cid`), not just the parent's key. (The shred SQL + unpin broadcast are the
   core tombstone procedure — M9.)
3. `image_metadata` rows need no action: `ON DELETE CASCADE` handles hard deletes; on tombstone
   they persist alongside the state-changed blob (the row is shredded/unpinned, not dropped).

M5 unit-tests the cascade directly (state transition + child-DEK enumerability); M9 wires the
caller and tests the end-to-end shred.

---

## Formats, codecs, and operator config

A govips wrapper maps `ext ↔ libvips loader/saver` with per-format encode params
(quality/effort; lossless for the spectral-bloat conversion). New `operator.yaml` `image:`
section (exact field names finalized in the plan — operator.yaml naming is a known M13
bikeshed; defaults shown):

```yaml
image:
  allowed_input_formats:  [jpeg, png, webp, gif, tiff, bmp]   # accepted at upload; add avif/jxl to accept them
  allowed_output_formats: [jpeg, png, webp]                   # offered via /i/*.{ext}; add avif/jxl to serve them
  allowed_widths:  [320, 512, 1024, 2048]                     # /i/{cid}/w{w}.{ext}; empty ⇒ presets-only
  allowed_boxes:   ["256x256", "1200x630"]                    # /i/{cid}/{w}x{h}.{ext}; empty ⇒ presets-only
  presets:
    thumb: { width: 256, format: webp }
    og:    { box: "1200x630", fit: cover, format: jpeg }
    hero:  { width: 1920, format: webp }
  prewarm_presets: [thumb, og]
  format_conversion: { enabled: false, target: webp, lossless: true }
  # resource bounds (see next section)
  max_megapixels:            100
  max_concurrent_transforms: 4
  vips_cache_max_mem_bytes:  134217728   # 128 MiB libvips C-heap cap
```

**AVIF/JXL are off by default as outputs**: present in the codebase and addable to
`allowed_output_formats`, but the operator opts in (JXL because browser delivery was unreliable
in 2026; both because encode is heavy). `allowed_input_formats` may include them so the store
*accepts and archives* JXL/AVIF even where it does not *serve* them by default.

### Transcoding fidelity

Lossy-source transcoding degrades quality, but the two paths carry very different stakes:

- **Read-time transcode (`/i/*`) is non-destructive and bounded.** Every derivative is rendered
  from the **canonical parent**, never from another derivative, so a lossy source incurs at most
  a **single** re-encode hop — not compounding generation loss — and the pristine original is
  always retained and re-servable. This is standard image-CDN behavior; a per-request,
  user-facing "quality may degrade" warning is **not** warranted (no client consumes one, and
  the loss is bounded and expected). Per-format encode defaults are chosen conservatively
  (e.g., WebP q80) and are operator-tunable; that is the right lever, not a warning.
- **Upload-time `format_conversion` replaces the stored original — the destructive case, and the
  one worth guarding.** It is deliberately scoped to **lossless inputs only** (PNG/BMP/TIFF) and
  defaults to **lossless** WebP, so the canonical bytes are preserved visually; it never
  transcodes a lossy source (an uploaded JPEG is stored as-is), and `collection.policy.
  preserve_original_format` disables it for byte-perfect archives. The one residual sharp edge —
  an operator setting `format_conversion.lossless: false`, which makes the canonical original a
  lossy re-encode — is a **config-time operator decision, not an uploader per-request one**, so
  it is surfaced as operator guidance (and the startup validator logs an explicit notice when
  destructive conversion is enabled), not as a runtime warning.

(An optional advisory `X-Nova-Transform` response header on transcoded derivatives is possible
for operator debugging, but it has no consumer today and is left out as YAGNI.)

---

## Resource bounds & abuse resistance

M5 is where untrusted bytes meet a native, memory-hungry, cgo image library on a public read
path. The bounds form one coherent budget:

| Bound | Knob | Defends against | Where enforced |
|---|---|---|---|
| Max object size | `uploads.max_upload_size_bytes` (M4) | oversized uploads | tus create, `Put`, multipart |
| Upload assembly concurrency | `uploads.max_concurrent_assembly` (M4) | upload RAM (envelope ~2×) | `Put` semaphore |
| **Max megapixels** | `image.max_megapixels` | **decompression bombs** (2 KB PNG → gigapixels; byte-size ≠ decode-RAM) | `Analyze` decode **and** transform decode |
| **Dimension whitelist** | `image.allowed_widths` / `allowed_boxes` | **Denial-of-Storage** (crawler enumerates `100x100,101x100,…` → unbounded permanent pinned blobs) | `/i/*` handler, **before** transform/persist; out-of-set → 400 |
| **Transform concurrency** | `image.max_concurrent_transforms` | CPU/native-RAM exhaustion from concurrent transforms (read **+** prewarm share one semaphore) | transform wrapper |
| **libvips cache cap** | `image.vips_cache_max_mem_bytes` | silent C-heap balloon → OOM-kill (Go GC is blind to cgo memory) | `vips.Startup` at boot |
| Single-flight + detached ctx | — | thundering herd on cold miss; wasted/aborted cgo work | `/i/*` + prewarm |

**Base image: Debian-slim (glibc), not Alpine (musl).** libvips' threaded, allocation-heavy
workload fragments badly under musl's allocator (the documented libvips/sharp-on-Alpine
degradation); glibc is the robust choice. (jemalloc-preload on Alpine is a known mitigation but
adds complexity for no benefit here — recorded as a rejected alternative.) The runtime image
ships a libvips compiled with the codecs the operator's allowed formats require.

---

## Authorization and the parent-aware visibility query

Derivatives hold **no collection membership**; they inherit the **parent's** visibility. The
naive implementation (resolve derivative row → read `parent_cid` → resolve parent row → resolve
parent memberships, sequentially) would be an N+1 on the hot read path. The design avoids the
chain two ways:

1. **The hot `/i/*` path keys on the parent CID from the URL** and authorizes the parent
   directly — there is no derivative→parent hop. A direct `/blob/{derivative_cid}` also does not
   chain: a derivative has no membership, so it resolves to private → 401 (safe by default).
2. **A single parent-aware visibility query** replaces M3's `ResolveBlobVisibility`, so any
   by-CID resolution (and the M8 `derivative_state_consistent` audit) resolves in one shot:

```sql
-- name: ResolveEffectiveVisibility :many
-- For an original, resolves its own memberships; for a derivative (parent_cid
-- NOT NULL) resolves the PARENT's, since derivatives inherit parent visibility.
SELECT c.visibility::text AS visibility
FROM blobs b
JOIN collection_items ci ON ci.blob_cid = COALESCE(b.parent_cid, b.cid)
JOIN collections c        ON c.id = ci.collection_id
WHERE b.cid = $1;
```

This also collapses, for originals, into the same single query M3 used. `Service.Resolve` is
updated to call it; behavior for non-derivative blobs is unchanged.

---

## Startup validation

Extends the M3/M4 refuse-to-start floor (precise message naming the offending key):

- **libvips codec capability detection.** At boot, `vips.Startup(...)` then query libvips for
  the load/save operations backing each format in `allowed_input_formats` /
  `allowed_output_formats`. **Refuse to start** if any allowed format is unavailable in the
  build, naming the missing codec (e.g., "allowed_output_formats includes `avif` but this
  libvips lacks the heif saver") — the operator's first debugging input, consistent with Nova's
  ethos. Closes the gap where config promises a format the build cannot produce.
- **libvips cache caps** applied at `vips.Startup` from `vips_cache_max_mem_bytes`.
- **Preset/whitelist sanity**: every preset's `format` ∈ `allowed_output_formats`; box strings
  parse; widths positive.
- **Destructive-conversion notice** (not a refusal): if `format_conversion.enabled` and
  `lossless: false`, log an explicit startup notice that the canonical stored original will be a
  lossy re-encode (the operator's deliberate choice — see "Transcoding fidelity").
- Existing floors (Kubo hardening, non-root, keystore bootstrap, anonymous-policy gate, tmp-dir
  writability) unchanged.

---

## HTTP contract

### Routes added

| Route | Owner | Notes |
|---|---|---|
| `GET /i/{cid}` | nova-image | original bytes; 415 if not image |
| `GET /i/{cid}.{ext}` | nova-image | transcode to `ext` (incl. `jxl` after reconciliation #4) |
| `GET /i/{cid}/w{w}.{ext}` | nova-image | width, aspect preserved; `w` ∈ `allowed_widths` |
| `GET /i/{cid}/{w}x{h}.{ext}` | nova-image | cover-crop box; `WxH` ∈ `allowed_boxes` |
| `GET /i/{cid}/p/{preset}.{ext}` | nova-image | named preset |
| `GET /i/{cid}.json` | nova-image | public image metadata (incl. width/height; `perceptual_hash` null) |
| `POST /api/v1/images` | core | multipart; `product='image'`; early 415 |

### Error → status

| Result | status | code |
|---|---|---|
| original / transform served | 200 | — (+ `X-Nova-Cid`, `X-Nova-Envelope-Version`, `X-Nova-Width/Height`) |
| image created (multipart) | 201 | `UploadResult` (+ `urls.presets`) |
| blob exists, not an image | 415 | `unsupported_media_type` |
| output `ext` not in `allowed_output_formats` | 406 | `format_not_allowed` |
| dynamic dimension not whitelisted | 400 | `dimension_not_allowed` |
| unknown preset / unknown cid | 404 | `not_found` |
| private parent (no signed URL / bearer) | 401 | `signed_url_required` |
| quarantined parent | 451 | `quarantined` |
| soft-deleted / tombstoned / key-shredded | 410 | `gone` |
| moderation reject (inert in P1) | 422 | `moderation_rejected` |
| decode failure / megapixel reject | 422 | `image_decode_failed` |
| transform / encrypt / import / commit failure | 500 | `internal` |

Reuses the existing `Error` schema + `WriteError` + request-id middleware. `/i/*` is mounted on
the chi router under the previously-reserved `/i` namespace; `/api/v1/images` joins the M4
`/api/v1` write subtree.

---

## Testing strategy

### Unit

- **transform wrapper** (requires libvips in the test env): width resize preserves aspect;
  `WxH` cover-crop produces exact dimensions; encode emits valid per-format magic bytes
  (jpeg/png/webp/gif; avif/jxl when the test libvips has them); format-conversion PNG→WebP is
  smaller and decodes; **megapixel reject**; output-format gating (disallowed ext → 406);
  dimension whitelist (out-of-set → 400; empty ⇒ presets-only).
- **`PutDerivative`** (testcontainer PG + fake `ipfs.Backend`): encrypted derivative round-trips
  via `OpenBytes`; `image_metadata` row (width/height, `perceptual_hash` NULL); `parent_cid`/
  `derivative_preset`/`derivative_format` set; not in `collection_items`; **unique-index loser
  unpins + returns the winner CID** (two concurrent same-key generations → one row, both callers
  get the winner; orphan unpinned exactly once).
- **parent-aware visibility**: original resolves own membership; derivative resolves the
  parent's; private parent ⇒ derivative private; one query (assert no per-row parent fetch).
- **write hook seam**: `Analyze` transform path swaps stored bytes/MIME/CID; `Scan.Action !=
  allow` ⇒ `ErrModerationRejected` (422), no commit; `Persist` runs in-tx (rollback drops the
  `image_metadata` row too); `OnCommitted` enqueues `derivative_prewarm`.
- **`OnDelete`**: bulk state cascade over `parent_cid`; child DEKs enumerable for shred.

### Integration (`internal/integration/m5_image_test.go`)

Testcontainer PG + real `EmbeddedBackend` (offline) + `Keystore`, coordinator in-process,
fronted by the **dev nginx** (M3 harness extended to pass `/i/*` + `/api/v1/images`). Seed a
public collection. Upload a JPEG/PNG via tus, multipart, **and** `/api/v1/images`; then:
`GET /i/{cid}/w512.webp` → **200**, a valid 512-px WebP (the **M5 exit criterion**);
`GET /i/{cid}/p/thumb.webp` hits the cache after first render; a second `w512.webp` is served
from the persisted derivative (one `blobs` row, not two). Prewarm: after upload, poll until
`thumb`+`og` derivative rows exist. Negatives: non-image blob via `/i/*` → 415; non-whitelisted
`w999.webp` → 400; disallowed `avif` output (default) → 406; private parent → 401. AVIF/JXL
paths exercised in a variant with them enabled (skipped if the CI libvips lacks the codec).

### CI

- **CI installs a codec-complete libvips** (jpeg/png/webp/gif/tiff, + libheif/aom for avif,
  + libjxl for jxl) in the `test` and `image-build` jobs — the notable CI change; govips will
  not build/link without it.
- `make codegen-check` continues to gate sqlc drift (now covering `InsertDerivativeBlob`,
  `GetDerivativeCID`, `ResolveEffectiveVisibility`). `test`/`vet`/`lint`/`schema-drift` unchanged.

---

## Security and privacy considerations

- **Denial-of-Storage closed** by the dimension whitelist (no unbounded permanent derivatives
  from dimension enumeration); out-of-whitelist requests do no work and persist nothing.
- **Decompression bombs** bounded by `max_megapixels` at every decode (upload + transform).
- **C-heap OOM** bounded by the libvips cache cap + transform-concurrency semaphore; glibc base
  avoids the musl fragmentation cliff.
- **Operator-visible plaintext is intended** (Tier 1): transforms decrypt the parent in-process
  under the operator's master key. No new trust boundary.
- **No new Kubo exposure**: derivative import is the same loopback `AddDeterministic`/`Unpin`.
- **Derivatives inherit parent visibility**; a private parent's derivatives are never publicly
  served, and a direct `/blob/{derivative_cid}` is private (401) by default.
- **`alt_text`/`caption`** are user-supplied and stored verbatim; they are metadata returned in
  JSON, never executed — consumers must escape on render (noted for the widget/admin SPA).
- **Anonymous writes remain dev-only** (M4 floor unchanged); nginx rate-limits bound abuse.
- **`perceptual_hash` is NULL** — no perceptual fingerprint is computed or stored in Phase 1
  (no upload-time content fingerprinting until the Phase 3/4 tracks land).

---

## Risks and notes

1. **libvips codec build is the critical path.** Codec availability varies by distro package;
   AVIF (libheif+aom) and JXL (libjxl) are the fragile ones. Mitigation: pin a Debian-slim base
   with a known codec-complete libvips (or build libvips), assert codecs at startup, keep CI at
   parity. **Validate this on day one of M5, before building transforms.**
2. **CGO memory is invisible to Go's GC.** Bounded structurally (cache cap + transform
   semaphore + megapixels); a libvips call cannot be `context`-cancelled, so bounds — not
   cancellation — are the protection, and generation uses a detached context so completed work
   commits.
3. **AVIF/JXL encode latency** on cold miss. Mitigation: prewarm common presets, cap dimensions,
   ship off-by-default.
4. **Single-flight is per-process** (Phase-1 single coordinator); the `(parent,preset,format)`
   unique index + loser-unpin is the Phase-2 cross-process backstop.
5. **Random-nonce derivatives** preclude CID dedup across regenerations; the unique index is the
   dedup key and the loser-unpin discipline keeps the blockstore clean (parallels M4's dedup
   note).
6. **JXL delivery was unreliable in 2026** (~12–17%, Safari-led); off-by-default output, positive
   trajectory (Chrome H2 2026, Firefox 152). Operators flip it on as their audience matures.
7. **Spec drift**: the six reconciliations above are applied to the affected docs; further gaps
   follow the project's v3.x amendment pattern.

---

## File structure

### Created in M5

| Path | Purpose |
|---|---|
| `pkg/coordinator/product/interface.go` | `Product`, `Metadata`, `UploadContext` (v0-unstable) |
| `pkg/coordinator/storage/scanresult.go` | `ScanResult` (Action/Rule/RuleRef/Notes) |
| `pkg/coordinator/storage/derivative.go` | `Service.PutDerivative` + `DerivativeContext` |
| `pkg/coordinator/storage/hook.go` | `WriteHook` / `AnalyzeResult` / `CommittedRef` seam types |
| `nova-image/product.go` | implements `product.Product`; registered in `cmd/coordinator` |
| `nova-image/internal/transform/transform.go` | govips wrapper (decode/resize/encode/format map/megapixel guard/cache+sem) |
| `nova-image/internal/transform/presets.go` | preset + dimension-whitelist resolution |
| `nova-image/internal/imageapi/routes.go` | `RegisterRoutes` + `/i/*` handlers (single-flight find-or-create) |
| `nova-image/internal/imagemeta/imagemeta.go` | `image_metadata` read/write (PDQ NULL) |
| `nova-image/internal/imagemoderation/scanner.go` | Phase-1 pass-through scanner |
| `nova-image/config.go` | nova-image config (formats/presets/bounds) |
| `nova-image/product_test.go`, transform/imageapi tests | unit tests |
| `internal/integration/m5_image_test.go` | nginx-fronted transform + prewarm round-trip |

### Modified in M5

| Path | Why |
|---|---|
| `pkg/coordinator/storage/put.go` | `WriteHook` call (Analyze/transform/scan/Persist/OnCommitted) |
| `pkg/coordinator/storage/blob.go` | parent-aware `ResolveEffectiveVisibility`; transform helpers |
| `pkg/coordinator/storage/types.go` / `errors.go` | `DerivativeContext`; new sentinels |
| `pkg/coordinator/coordinator.go` | `RegisterProduct`; product→`WriteHook` adapter; start worker pool |
| `internal/api/server.go` | mount `/api/v1/images`; product route mounting |
| `internal/api/handlers/upload.go` | `/api/v1/images` (force `product=image`, early 415); `urls.presets` |
| `internal/jobs/kinds/derivative_prewarm.go` | real handler body (find-or-create per preset) |
| `cmd/coordinator/main.go` | register nova-image; load `image:` config; `vips.Startup` + codec validation |
| `internal/config/types.go` + `operator_yaml.go` | `Image` config section + defaults |
| `internal/db/queries/writes.sql` | `InsertDerivativeBlob`, `GetDerivativeCID` |
| `internal/db/queries/collections.sql` | `ResolveEffectiveVisibility` (parent-aware; replaces `ResolveBlobVisibility`) |
| `internal/db/gen/*` | regenerated (committed) |
| `docker/nginx/nova.dev.conf` | pass `/i/*` + `/api/v1/images` |
| `go.mod` / `go.sum` | + govips (first cgo dep) |
| `.github/workflows/ci.yml` | install codec-complete libvips in `test` + `image-build` |
| `docs/specs/openapi.yaml` | add `jxl` to `ext` enums; note dimension-whitelist 400 on `w/h` and 406 on operator-disallowed output formats |
| `docs/specs/PRODUCT_MODULE_INTERFACE.md` | image_metadata core-owned note; PDQ two-track |
| `docs/specs/DATA_MODEL.sql` + `internal/db/migrations/0001_init.sql` | `perceptual_hash` comment (two-track; no schema change) |
| `docs/ROADMAP.md` | Phase 3 pHash / Phase 4 PDQ naming |
| `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md` | resolve base-image to Debian-slim; worker-pool startup → M5 |
| `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md` | mark M5 in progress; link M5 plan |

---

## Cross-references

- `docs/superpowers/specs/2026-05-29-phase1-m4-upload-pipeline-design.md` — the write path M5
  extends (the `Put` transaction, the `AnalyzeUpload` no-op seam, dedup/unpin discipline).
- `docs/superpowers/specs/2026-05-28-phase1-m3-storage-read-api-design.md` — the read path,
  visibility/state mapping, and dev-nginx harness M5 reuses.
- `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md` — Phase-1 architecture
  (transform pipeline §, derivative §, container topology, M5 milestone line).
- `docs/specs/PRODUCT_MODULE_INTERFACE.md` — the `Product` contract M5 implements; the
  derivative procedure; reconciliations #1 and #2.
- `docs/specs/DATA_MODEL.sql` — `blobs` derivative columns, `blobs_derivative_lookup_idx`,
  `image_metadata` (all pre-existing in `0001_init`).
- `docs/specs/openapi.yaml` — `/i/*`, `/api/v1/images`, `UploadResult`, `Image`, `ImageBytes`.
- `docs/specs/IPFS_IMPORT_RULES.md` — deterministic import parameters derivative writes honor.
- `docs/specs/ENCRYPTION_ENVELOPE.md` — v1 single-shot encrypt + per-blob key wrap.
- `docs/specs/ARCHITECTURE_DECISIONS.md` — Tier 1 (operator-visible plaintext on transform).
