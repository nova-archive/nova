# M5 Image Transforms Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `nova-image` into a real product — `GET /i/{cid}/w512.webp` against an uploaded JPEG returns a 512-px WebP persisted as a first-class derivative blob, common presets pre-warm on upload, and the generic `product.Product` interface + `RegisterProduct` wiring lands.

**Architecture:** nova-image implements the v0-unstable `pkg/coordinator/product.Product`; the storage core's `Service.Put` calls an injected `storage.WriteHook` seam (so `storage` never imports `product`); `Service.PutDerivative` mirrors `Put` for derivatives; `/i/*` handlers do single-flight find-or-create through a bounded govips pipeline; `coordinator.Run` starts the (M2) worker pool for the `derivative_prewarm` handler.

**Tech Stack:** Go 1.26, chi, pgx/sqlc, testcontainers (`internal/dbtest`), embedded Kubo (`internal/ipfs`), **govips/libvips (first cgo dependency)**, testify.

**Spec:** `docs/superpowers/specs/2026-05-29-phase1-m5-image-transforms-design.md` (authoritative).

**Conventions (match the existing codebase):**
- Integration tests guard with `if testing.Short() { t.Skip("integration") }` and use `dbtest.New(t, ctx)`.
- Reuse `fakeBackend`, `bootstrapKS`, `seedCollection` from `pkg/coordinator/storage/put_test.go` (promote shared helpers to a `_test.go` in-package; do not export).
- Commit messages: `feat(scope): …` / `test(scope): …` / `docs(…): …`, ending with the `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>` trailer. Stay on branch `m5-image-transforms`.
- `make codegen-check` must pass after any `internal/db/queries/*.sql` change (run `sqlc generate`, commit `internal/db/gen/*`).
- Do not reformat pre-existing files (host gofmt is newer than the repo toolchain); `gofmt -w` only files you create/edit.

---

## File Structure

**Created**
- `pkg/coordinator/product/interface.go` — `Product`, `Metadata`, `UploadContext` (v0-unstable).
- `pkg/coordinator/storage/scanresult.go` — `ScanResult`, `ActionAllow`.
- `pkg/coordinator/storage/hook.go` — `WriteHook`, `AnalyzeResult`, `CommittedRef`.
- `pkg/coordinator/storage/derivative.go` — `Service.PutDerivative`, `DerivativeContext`.
- `nova-image/config.go` — nova-image `Config` + defaults + `Validate`.
- `nova-image/internal/transform/transform.go` — govips wrapper (Startup, Decode, Render, format map, bounds).
- `nova-image/internal/transform/presets.go` — preset + dimension-whitelist resolution.
- `nova-image/internal/imagemeta/imagemeta.go` — `image_metadata` read/write (perceptual_hash NULL).
- `nova-image/internal/imagemoderation/scanner.go` — Phase-1 pass-through scanner.
- `nova-image/internal/imageapi/imageapi.go` — `/i/*` handlers + single-flight find-or-create + prewarm fn.
- `nova-image/product.go` — implements `product.Product`.
- `internal/integration/m5_image_test.go` — nginx-fronted transform + prewarm round-trip.

**Modified**
- `pkg/coordinator/storage/blob.go` — `Service` gains `hook`; `WithProductHook`; parent-aware visibility.
- `pkg/coordinator/storage/put.go` — call `hook.Analyze` / Persist / `OnCommitted`.
- `pkg/coordinator/storage/types.go`, `errors.go` — `DerivativeContext` ref; new sentinels.
- `internal/db/queries/collections.sql` — `ResolveEffectiveVisibility` (replaces `ResolveBlobVisibility`).
- `internal/db/queries/writes.sql` — `InsertDerivativeBlob`, `GetDerivativeCID`; `internal/db/gen/*` regen.
- `pkg/coordinator/coordinator.go` — `RegisterProduct`, product→`WriteHook` adapter, worker pool start.
- `internal/api/server.go`, `internal/api/handlers/upload.go` — `/api/v1/images`; product route mounting; `urls.presets`.
- `internal/jobs/kinds/derivative_prewarm.go` — real handler (`NewHandler(prewarmFn)` + payload).
- `cmd/coordinator/main.go` — register nova-image; load `image:` config; `vips.Startup` + codec validation.
- `internal/config/types.go`, `operator_yaml.go` — `Image` section + defaults.
- `docker/nginx/nova.dev.conf` — pass `/i/*` + `/api/v1/images`.
- `go.mod`, `go.sum` — `+ github.com/davidbyttow/govips/v2`.
- `.github/workflows/ci.yml` — install codec-complete libvips.
- Docs (Task 18): `openapi.yaml`, `PRODUCT_MODULE_INTERFACE.md`, `DATA_MODEL.sql`, `0001_init.sql`, `ROADMAP.md`, the MVP design + master plan.

---

## Task 0: Environment + govips dependency (validate the cgo cliff first)

The spec's #1 risk is the libvips/codec build. Prove the cgo link before building anything on it.

**Files:** `go.mod`, `go.sum`, scratch `nova-image/internal/transform/smoke_test.go` (deleted at end of task).

- [ ] **Step 1: Install a codec-complete libvips on the host.**

Arch (this host): `sudo pacman -S --needed libvips` then verify codecs:
Run: `vips --version && vips --vips-config | grep -iE "jpeg|png|webp|heif|jxl"`
Expected: a version line; `webp`, `jpeg`, `png` present. `heif` (AVIF) and `jxl` may be absent — note which; the AVIF/JXL tests skip when their saver is unavailable (Task 8).

- [ ] **Step 2: Add govips.**

Run: `go get github.com/davidbyttow/govips/v2@latest && go mod tidy`
Expected: `go.mod` gains `github.com/davidbyttow/govips/v2`.

- [ ] **Step 3: Write a smoke test that links libvips and round-trips a resize.**

```go
package transform

import (
	"testing"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/stretchr/testify/require"
)

func TestCgoSmoke(t *testing.T) {
	vips.Startup(nil)
	defer vips.Shutdown()
	// 2x2 red PNG.
	img, err := vips.NewImageFromBuffer([]byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x02, 0x08, 0x02, 0x00, 0x00, 0x00, 0xfd, 0xd4, 0x9a,
		0x73, 0x00, 0x00, 0x00, 0x16, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x62, 0xf8, 0xcf, 0xc0, 0xf0,
		0x1f, 0x08, 0x18, 0x19, 0x18, 0x18, 0xfe, 0x03, 0x00, 0x0b, 0xf0, 0x02, 0xfe, 0x4a, 0x6c, 0x8e,
		0x9e, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	})
	require.NoError(t, err)
	defer img.Close()
	require.NoError(t, img.Resize(2.0, vips.KernelLanczos3))
	require.Equal(t, 4, img.Width())
}
```

- [ ] **Step 4: Run it.**

Run: `go test ./nova-image/internal/transform/ -run TestCgoSmoke -v`
Expected: PASS. If it fails to link, resolve libvips install before continuing (this is the day-one gate).

- [ ] **Step 5: Delete the smoke test, commit the dependency.**

```bash
rm nova-image/internal/transform/smoke_test.go
git add go.mod go.sum
git commit -m "build(m5): add govips (first cgo dep); validate libvips link"
```

---

## Task 1: Product interface + ScanResult (pure Go)

**Files:**
- Create: `pkg/coordinator/storage/scanresult.go`
- Create: `pkg/coordinator/product/interface.go`
- Test: `pkg/coordinator/product/interface_test.go`

- [ ] **Step 1: Write the failing test.**

```go
package product_test

import (
	"testing"

	"github.com/nova-archive/nova/pkg/coordinator/product"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

func TestScanResultAllowConst(t *testing.T) {
	require.Equal(t, "allow", string(storage.ActionAllow))
}

// imageMetaStub proves a product Metadata is implementable.
type imageMetaStub struct{}

func (imageMetaStub) ProductName() string { return "image" }

func TestMetadataImplements(t *testing.T) {
	var m product.Metadata = imageMetaStub{}
	require.Equal(t, "image", m.ProductName())
}
```

- [ ] **Step 2: Run — expect FAIL** (`undefined: storage.ActionAllow`, `product.Metadata`).

Run: `go test ./pkg/coordinator/product/ -v`

- [ ] **Step 3: Create `scanresult.go`.**

```go
package storage

// ScanAction is the moderation verdict a product returns from analysis.
type ScanAction string

const (
	ActionAllow      ScanAction = "allow"
	ActionQuarantine ScanAction = "quarantine"
	ActionTombstone  ScanAction = "tombstone"
)

// ScanResult is a product's synchronous moderation verdict on a plaintext
// upload. Phase 1 nova-image always returns ActionAllow (manual moderation).
type ScanResult struct {
	Action  ScanAction
	Rule    string // e.g. "pdq_match" (Phase 3/4)
	RuleRef string // blocklist entry id, etc.
	Notes   string
}
```

- [ ] **Step 4: Create `product/interface.go`** (the spec's interface verbatim; `Metadata` gains a transactional `Persist` so products own their side-table write — v0, allowed).

```go
// Package product defines the content-type-specific product layer interface.
// It is part of pkg/coordinator's public surface but v0.x.y UNSTABLE until
// Phase 4 adapters are real consumers. storage MUST NOT import this package
// (the storage.WriteHook seam inverts the dependency).
package product

import (
	"context"
	"io"
	"io/fs"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// Product is registered with the coordinator at boot; one singleton per process.
type Product interface {
	Name() string
	AcceptedMimeTypes() []string

	// AnalyzeUpload runs on plaintext before encryption. plaintext is valid for
	// the call only; the product MUST NOT retain it. A non-nil transformedPlaintext
	// is encrypted/stored instead of the original. scan.Action != ActionAllow
	// rejects the upload (no state committed).
	AnalyzeUpload(ctx context.Context, uc *UploadContext, plaintext io.Reader) (
		metadata Metadata, scan *storage.ScanResult, transformedPlaintext io.Reader, err error)

	OnCommitted(ctx context.Context, blob *storage.CommittedRef, metadata Metadata) error
	OnDelete(ctx context.Context, tx pgx.Tx, parentCID string, newState string) error
	RegisterRoutes(r chi.Router)
	Migrations() (fs.FS, string)
}

// UploadContext carries declared metadata from the upload request.
type UploadContext struct {
	DeclaredMimeType string
	Filename         string
	CollectionID     *string
	OwnerID          *string
	SourceIP         string
}

// Metadata is product-specific side-table metadata. Persist writes the row
// inside the storage core's write transaction (products own their side tables).
type Metadata interface {
	ProductName() string
	Persist(ctx context.Context, tx pgx.Tx, cid string) error
}
```

- [ ] **Step 5: Run — expect PASS; then `gofmt -w` + commit.**

Run: `go test ./pkg/coordinator/product/ -v`
```bash
gofmt -w pkg/coordinator/storage/scanresult.go pkg/coordinator/product/interface.go pkg/coordinator/product/interface_test.go
git add pkg/coordinator/product/ pkg/coordinator/storage/scanresult.go
git commit -m "feat(product): v0-unstable Product interface + storage.ScanResult"
```

---

## Task 2: storage.WriteHook seam (pure Go)

**Files:**
- Create: `pkg/coordinator/storage/hook.go`
- Modify: `pkg/coordinator/storage/blob.go` (Service field + `WithProductHook`)

- [ ] **Step 1: Create `hook.go`** (storage-local seam — no `product` import).

```go
package storage

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// CommittedRef is the post-commit view passed to OnCommitted (best-effort hooks).
type CommittedRef struct {
	CID        string
	Product    string
	Visibility Visibility
}

// AnalyzeResult is what a WriteHook returns from Analyze.
type AnalyzeResult struct {
	Scan        ScanResult
	Transformed []byte                                                  // nil ⇒ store the original
	ResultMIME  string                                                  // set iff Transformed != nil
	Persist     func(ctx context.Context, tx pgx.Tx, cid string) error  // side-table write in Put's tx; nil ⇒ none
}

// WriteHook is the product seam Service.Put calls. The coordinator adapts a
// product.Product to this interface (so storage never imports product).
type WriteHook interface {
	Analyze(ctx context.Context, pc PutContext, plaintext []byte) (AnalyzeResult, error)
	OnCommitted(ctx context.Context, ref CommittedRef)
}
```

- [ ] **Step 2: Add the field + option to `blob.go`.** Add `hook WriteHook` to `Service`; add to `svcOpts`/`NewService`:

```go
// in svcOpts: add `hook WriteHook`
// new Option:
func WithProductHook(h WriteHook) Option { return func(o *svcOpts) { o.hook = h } }
// in NewService: set `hook: o.hook` on the returned &Service{...}
```

Add `hook WriteHook` to the `Service` struct.

- [ ] **Step 3: Build.**

Run: `go build ./pkg/coordinator/storage/`
Expected: builds clean (no behavior change yet).

- [ ] **Step 4: Commit.**

```bash
gofmt -w pkg/coordinator/storage/hook.go pkg/coordinator/storage/blob.go
git add pkg/coordinator/storage/hook.go pkg/coordinator/storage/blob.go
git commit -m "feat(storage): WriteHook seam + WithProductHook option"
```

---

## Task 3: Parent-aware visibility query

**Files:**
- Modify: `internal/db/queries/collections.sql`
- Modify: `internal/db/gen/*` (regen)
- Modify: `pkg/coordinator/storage/blob.go` (call the new query)
- Test: `pkg/coordinator/storage/visibility_test.go`

- [ ] **Step 1: Write the failing test** (derivative inherits parent visibility via ONE query).

```go
func TestIntegrationDerivativeInheritsParentVisibility(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	col := seedCollection(t, ctx, pool, "public", false)

	// Parent original in the public collection.
	_, err := pool.Exec(ctx, `INSERT INTO blobs (cid, mime_type, byte_size, product) VALUES ('bafyparent','image/jpeg',10,'image')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO collection_items (collection_id, blob_cid) VALUES ($1,'bafyparent')`, col)
	require.NoError(t, err)
	// Derivative: parent_cid set, NO collection membership of its own.
	_, err = pool.Exec(ctx, `INSERT INTO blobs (cid, parent_cid, derivative_preset, derivative_format, mime_type, byte_size, product)
		VALUES ('bafyderiv','bafyparent','thumb','webp','image/webp',5,'image')`)
	require.NoError(t, err)

	q := gen.New(pool)
	vis, err := q.ResolveEffectiveVisibility(ctx, "bafyderiv")
	require.NoError(t, err)
	require.Equal(t, []string{"public"}, vis)
}
```

- [ ] **Step 2: Run — expect FAIL** (`undefined: q.ResolveEffectiveVisibility`).

Run: `go test ./pkg/coordinator/storage/ -run DerivativeInheritsParent -v`

- [ ] **Step 3: Replace the query in `collections.sql`.**

```sql
-- name: ResolveEffectiveVisibility :many
-- For an original, resolves its own collection memberships; for a derivative
-- (parent_cid NOT NULL) resolves the PARENT's, since derivatives inherit
-- parent visibility and hold no membership of their own. One query, no N+1.
SELECT c.visibility::text AS visibility
FROM blobs b
JOIN collection_items ci ON ci.blob_cid = COALESCE(b.parent_cid, b.cid)
JOIN collections c        ON c.id = ci.collection_id
WHERE b.cid = $1;
```

(Remove the old `ResolveBlobVisibility`.)

- [ ] **Step 4: Regenerate + update the caller.**

Run: `sqlc generate -f internal/db/sqlc.yaml` (or `make sqlc`).
In `blob.go` `Resolve`, change `s.q.ResolveBlobVisibility(ctx, cidStr)` → `s.q.ResolveEffectiveVisibility(ctx, cidStr)`.

- [ ] **Step 5: Run the new test + the existing visibility suite + codegen-check.**

Run: `go test ./pkg/coordinator/storage/ -run Visibility -v && go test ./pkg/coordinator/storage/ -run DerivativeInheritsParent -v && make codegen-check`
Expected: PASS; codegen-check clean.

- [ ] **Step 6: Commit.**

```bash
git add internal/db/queries/collections.sql internal/db/gen pkg/coordinator/storage/blob.go pkg/coordinator/storage/visibility_test.go
git commit -m "feat(storage): parent-aware ResolveEffectiveVisibility (no derivative N+1)"
```

---

## Task 4: Derivative write queries

**Files:**
- Modify: `internal/db/queries/writes.sql`
- Modify: `internal/db/gen/*` (regen)

- [ ] **Step 1: Add the queries.**

```sql
-- name: InsertDerivativeBlob :execrows
-- Inserts a derivative blob. ON CONFLICT on the (parent,preset,format) partial
-- unique index DO NOTHING ⇒ 0 rows when a concurrent/cross-process writer won
-- (the caller then unpins its orphan import and reads the winner).
INSERT INTO blobs (cid, encryption_key_id, parent_cid, derivative_preset, derivative_format,
                   mime_type, byte_size, product, state, envelope_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'image', 'active', 1)
ON CONFLICT (parent_cid, derivative_preset, derivative_format) WHERE parent_cid IS NOT NULL
DO NOTHING;

-- name: GetDerivativeCID :one
SELECT cid FROM blobs
WHERE parent_cid = $1 AND derivative_preset = $2 AND derivative_format = $3;
```

- [ ] **Step 2: Regenerate.**

Run: `sqlc generate -f internal/db/sqlc.yaml`
Expected: `InsertDerivativeBlob` returns `(int64, error)` (`:execrows`); `GetDerivativeCID` returns `(string, error)`.

- [ ] **Step 3: Verify build + codegen-check.**

Run: `go build ./internal/db/... && make codegen-check`

- [ ] **Step 4: Commit.**

```bash
git add internal/db/queries/writes.sql internal/db/gen
git commit -m "feat(db): derivative write + lookup queries (committed gen)"
```

---

## Task 5: Service.PutDerivative

**Files:**
- Create: `pkg/coordinator/storage/derivative.go`
- Modify: `pkg/coordinator/storage/errors.go` (no new sentinel needed unless absent; reuse existing)
- Test: `pkg/coordinator/storage/derivative_test.go`

- [ ] **Step 1: Write the failing tests.**

```go
func insertParent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, col uuid.UUID, cidStr string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO blobs (cid, mime_type, byte_size, product) VALUES ($1,'image/jpeg',10,'image')`, cidStr)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO collection_items (collection_id, blob_cid) VALUES ($1,$2)`, col, cidStr)
	require.NoError(t, err)
}

func TestIntegrationPutDerivativeRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := NewService(pool, fb, ks)
	col := seedCollection(t, ctx, pool, "public", false)
	insertParent(t, ctx, pool, col, "bafyparentA")

	persist := func(ctx context.Context, tx pgx.Tx, cid string) error {
		_, err := tx.Exec(ctx, `INSERT INTO image_metadata (cid,width,height,perceptual_hash) VALUES ($1,512,384,NULL)`, cid)
		return err
	}
	out := []byte("transformed-webp-bytes")
	res, err := svc.PutDerivative(ctx, out, DerivativeContext{
		ParentCID: "bafyparentA", Preset: "w512", Format: "webp", MIME: "image/webp", Width: 512, Height: 384,
	}, persist)
	require.NoError(t, err)
	require.True(t, res.Encrypted)

	// Derivative decrypts back to the transformed bytes.
	view, err := svc.Resolve(ctx, res.CID) // inherits parent's public visibility
	require.NoError(t, err)
	rc, err := svc.OpenBytes(ctx, view)
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	require.Equal(t, out, got)

	// image_metadata row exists with NULL perceptual_hash.
	var w, h int
	var ph []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT width,height,perceptual_hash FROM image_metadata WHERE cid=$1`, res.CID).Scan(&w, &h, &ph))
	require.Equal(t, 512, w)
	require.Nil(t, ph)

	// Lookup finds it.
	dcid, err := gen.New(pool).GetDerivativeCID(ctx, gen.GetDerivativeCIDParams{ParentCid: ptr("bafyparentA"), DerivativePreset: ptr("w512"), DerivativeFormat: ptr("webp")})
	require.NoError(t, err)
	require.Equal(t, res.CID, dcid)
}

func TestIntegrationPutDerivativeConflictLoserUnpins(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := NewService(pool, fb, ks)
	col := seedCollection(t, ctx, pool, "public", false)
	insertParent(t, ctx, pool, col, "bafyparentB")
	persist := func(ctx context.Context, tx pgx.Tx, cid string) error {
		_, err := tx.Exec(ctx, `INSERT INTO image_metadata (cid,width,height) VALUES ($1,512,384)`, cid)
		return err
	}
	dc := DerivativeContext{ParentCID: "bafyparentB", Preset: "w512", Format: "webp", MIME: "image/webp", Width: 512, Height: 384}

	res1, err := svc.PutDerivative(ctx, []byte("first-bytes"), dc, persist)
	require.NoError(t, err)
	// Second generation of the SAME (parent,preset,format) — different envelope CID
	// (random nonce) but the unique index makes it a no-op winner-return.
	res2, err := svc.PutDerivative(ctx, []byte("second-different-bytes"), dc, persist)
	require.NoError(t, err)
	require.Equal(t, res1.CID, res2.CID, "loser returns the winner's CID")
	require.NotEmpty(t, fb.unpinned, "loser unpinned its orphan import")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE parent_cid='bafyparentB'`).Scan(&n))
	require.Equal(t, 1, n)
}
```

(Add `func ptr[T any](v T) *T { return &v }` to the test file if not already present, and confirm the gen param/field names after Task 4 regen — adjust `ParentCid`/`DerivativePreset`/`DerivativeFormat` to match sqlc's emitted names.)

- [ ] **Step 2: Run — expect FAIL** (`undefined: svc.PutDerivative`).

Run: `go test ./pkg/coordinator/storage/ -run PutDerivative -v`

- [ ] **Step 3: Implement `derivative.go`.**

```go
package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
)

// DerivativeContext carries validated derivative-write metadata.
type DerivativeContext struct {
	ParentCID string
	Preset    string // canonical key part: 'thumb' | 'w512' | '512x384'
	Format    string // 'webp' | 'jpeg' | 'png' | 'avif' | 'jxl'
	MIME      string
	Width     int
	Height    int
}

// PutDerivative encrypts a derivative under a fresh per-blob key, imports it
// deterministically, and commits blobs(+derivative cols) + manifest + blocks +
// DEK in one transaction; persist (non-nil) writes the product side-table row
// in the same tx. Idempotent under the (parent,preset,format) unique index: a
// loser unpins its orphan import and returns the winner's CID. The assembly
// semaphore bounds in-memory work, same as Put.
func (s *Service) PutDerivative(ctx context.Context, plaintext []byte, dc DerivativeContext,
	persist func(ctx context.Context, tx pgx.Tx, cid string) error) (*PutResult, error) {

	select {
	case s.assembly <- struct{}{}:
		defer func() { <-s.assembly }()
	default:
		return nil, ErrServerBusy
	}

	pbk := make([]byte, envelope.KeySize)
	if _, err := rand.Read(pbk); err != nil {
		return nil, fmt.Errorf("storage: derivative key: %w", err)
	}
	wrapped, mkvID, err := s.ks.Wrap(pbk)
	if err != nil {
		return nil, fmt.Errorf("storage: wrap: %w", err)
	}
	env, err := envelope.V1().Encrypt(plaintext, pbk)
	if err != nil {
		return nil, fmt.Errorf("storage: encrypt: %w", err)
	}
	add, err := s.backend.AddDeterministic(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("storage: import: %w", err)
	}

	won, err := s.commitDerivative(ctx, add, plaintext, dc, wrapped, mkvID, persist)
	if err != nil {
		if uerr := s.backend.Unpin(ctx, add.CID); uerr != nil {
			err = fmt.Errorf("%w (unpin failed: %v)", err, uerr)
		}
		return nil, fmt.Errorf("storage: derivative commit: %w", err)
	}
	if !won {
		// Lost the (parent,preset,format) race: unpin our orphan, return winner.
		_ = s.backend.Unpin(ctx, add.CID)
		winner, gerr := s.q.GetDerivativeCID(ctx, gen.GetDerivativeCIDParams{
			ParentCid: &dc.ParentCID, DerivativePreset: &dc.Preset, DerivativeFormat: &dc.Format,
		})
		if gerr != nil {
			return nil, fmt.Errorf("storage: lookup winner: %w", gerr)
		}
		return &PutResult{CID: winner, ByteSize: int64(len(plaintext)), MIME: dc.MIME, Product: "image", Encrypted: true}, nil
	}
	return &PutResult{CID: add.CID.String(), ByteSize: int64(len(plaintext)), MIME: dc.MIME, Product: "image", Encrypted: true}, nil
}

// commitDerivative returns won=false when the (parent,preset,format) unique
// index rejected the insert (0 rows) — the caller recovers gracefully.
func (s *Service) commitDerivative(ctx context.Context, add ipfsAdd, plaintext []byte, dc DerivativeContext,
	wrapped []byte, mkvID uuid.UUID, persist func(context.Context, pgx.Tx, string) error) (bool, error) {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)
	cidStr := add.CID.String()

	keyID, err := qtx.InsertDEK(ctx, gen.InsertDEKParams{
		Algorithm: "XChaCha20-Poly1305", WrappedKey: wrapped, MasterKeyVersionID: pgUUID(mkvID),
	})
	if err != nil {
		return false, err
	}
	rows, err := qtx.InsertDerivativeBlob(ctx, gen.InsertDerivativeBlobParams{
		Cid: cidStr, EncryptionKeyID: keyID,
		ParentCid: &dc.ParentCID, DerivativePreset: &dc.Preset, DerivativeFormat: &dc.Format,
		MimeType: dc.MIME, ByteSize: int64(len(plaintext)),
	})
	if err != nil {
		return false, err
	}
	if rows == 0 {
		// Unique-index conflict: roll back (defer) and signal the loser path.
		return false, nil
	}
	var mr pgtype.Text
	if len(add.Blocks) > 1 {
		mr = pgtype.Text{String: add.MerkleRoot.String(), Valid: true}
	}
	if err := qtx.InsertManifest(ctx, gen.InsertManifestParams{
		Cid: cidStr, HashAlg: "sha2-256", Codec: add.Codec, Chunker: "size-262144",
		PlaintextSize: int64(len(plaintext)), EnvelopeSize: add.EnvelopeSize,
		BlockCount: int32(len(add.Blocks)), MerkleRoot: mr,
	}); err != nil {
		return false, err
	}
	for _, b := range add.Blocks {
		if err := qtx.InsertBlock(ctx, gen.InsertBlockParams{
			BlobCid: cidStr, BlockCid: b.CID.String(), BlockIndex: int32(b.Index), BlockSize: int32(b.Size),
		}); err != nil {
			return false, err
		}
	}
	if persist != nil {
		if err := persist(ctx, tx, cidStr); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
```

Note: `ipfsAdd` is `ipfs.AddResult` — import `"github.com/nova-archive/nova/internal/ipfs"` and use `ipfs.AddResult` (the alias is illustrative; use the real type). `EncryptionKeyID` is `pgtype.UUID` (the value returned by `InsertDEK`). Confirm sqlc's nullable-text param Go type after Task 4 regen — pgx v5 + sqlc emits `*string` for nullable `text`; adjust `&dc.ParentCID` accordingly.

- [ ] **Step 4: Run — expect PASS** (both tests).

Run: `go test ./pkg/coordinator/storage/ -run PutDerivative -v`

- [ ] **Step 5: Commit.**

```bash
gofmt -w pkg/coordinator/storage/derivative.go pkg/coordinator/storage/derivative_test.go
git add pkg/coordinator/storage/derivative.go pkg/coordinator/storage/derivative_test.go
git commit -m "feat(storage): PutDerivative (fresh-key encrypt+import+commit; loser-unpin on lookup conflict)"
```

---

## Task 6: Wire the WriteHook into Service.Put

**Files:**
- Modify: `pkg/coordinator/storage/put.go`
- Modify: `pkg/coordinator/storage/errors.go` (+ `ErrModerationRejected`)
- Test: `pkg/coordinator/storage/put_hook_test.go`

- [ ] **Step 1: Add `ErrModerationRejected` to `errors.go`.**

```go
ErrModerationRejected = errors.New("storage: upload rejected by moderation")
```

- [ ] **Step 2: Write the failing test** (fake hook: transform-swap, scan-reject, Persist-in-tx, OnCommitted).

```go
type fakeHook struct {
	transformTo []byte
	mime        string
	action      ScanAction
	committed   []string
}

func (h *fakeHook) Analyze(ctx context.Context, pc PutContext, plaintext []byte) (AnalyzeResult, error) {
	ar := AnalyzeResult{Scan: ScanResult{Action: h.action}}
	if h.transformTo != nil {
		ar.Transformed = h.transformTo
		ar.ResultMIME = h.mime
	}
	ar.Persist = func(ctx context.Context, tx pgx.Tx, cid string) error {
		_, err := tx.Exec(ctx, `INSERT INTO image_metadata (cid,width,height) VALUES ($1,1,1)`, cid)
		return err
	}
	return ar, nil
}
func (h *fakeHook) OnCommitted(ctx context.Context, ref CommittedRef) { h.committed = append(h.committed, ref.CID) }

func TestIntegrationPutHookTransformAndPersist(t *testing.T) {
	if testing.Short() { t.Skip("integration") }
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	h := &fakeHook{transformTo: []byte("webp!"), mime: "image/webp", action: ActionAllow}
	svc := NewService(pool, fb, ks, WithProductHook(h))
	col := seedCollection(t, ctx, pool, "public", false)

	res, err := svc.Put(ctx, bytes.NewReader([]byte("original-png")), int64(len("original-png")),
		PutContext{MIME: "image/png", Product: "image", CollectionID: &col})
	require.NoError(t, err)
	require.Equal(t, "image/webp", res.MIME, "stored mime is the transformed one")
	view, _ := svc.Resolve(ctx, res.CID)
	rc, _ := svc.OpenBytes(ctx, view)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	require.Equal(t, []byte("webp!"), got, "stored bytes are the transformed ones")
	require.Equal(t, []string{res.CID}, h.committed)

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM image_metadata WHERE cid=$1`, res.CID).Scan(&n))
	require.Equal(t, 1, n)
}

func TestIntegrationPutHookModerationReject(t *testing.T) {
	if testing.Short() { t.Skip("integration") }
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	h := &fakeHook{action: ActionTombstone}
	svc := NewService(pool, fb, ks, WithProductHook(h))
	col := seedCollection(t, ctx, pool, "public", false)
	_, err := svc.Put(ctx, bytes.NewReader([]byte("bad")), 3, PutContext{MIME: "image/png", Product: "image", CollectionID: &col})
	require.ErrorIs(t, err, ErrModerationRejected)
	require.Empty(t, fb.store, "nothing imported on reject")
}
```

- [ ] **Step 3: Run — expect FAIL.**

Run: `go test ./pkg/coordinator/storage/ -run PutHook -v`

- [ ] **Step 4: Edit `put.go`.** After the `validateMIME` block and before the `encrypt` decision, insert the hook call; thread a `persist` into `commit`; call `OnCommitted` after the commit succeeds.

```go
	// (after: mime, err := validateMIME(...) … )
	var persist func(context.Context, pgx.Tx, string) error
	if s.hook != nil {
		ar, herr := s.hook.Analyze(ctx, pc, buf)
		if herr != nil {
			return nil, herr
		}
		if ar.Scan.Action != ActionAllow {
			return nil, ErrModerationRejected
		}
		if ar.Transformed != nil {
			buf = ar.Transformed
			if ar.ResultMIME != "" {
				mime = ar.ResultMIME
			}
			if int64(len(buf)) > s.maxUploadSize {
				return nil, ErrUploadTooLarge
			}
		}
		persist = ar.Persist
	}
```

Change `commit(...)` to accept `persist` and call it after the collection-item insert (mirror the `commitDerivative` persist call). After `s.commit(...)` returns nil, before building `PutResult`, call:

```go
	if s.hook != nil {
		s.hook.OnCommitted(ctx, CommittedRef{CID: add.CID.String(), Product: pc.Product})
	}
```

(Add `"github.com/jackc/pgx/v5"` to `put.go` imports for the `persist` type.)

- [ ] **Step 5: Run — expect PASS; full storage suite green.**

Run: `go test ./pkg/coordinator/storage/ -v`

- [ ] **Step 6: Commit.**

```bash
gofmt -w pkg/coordinator/storage/put.go pkg/coordinator/storage/errors.go pkg/coordinator/storage/put_hook_test.go
git add pkg/coordinator/storage/put.go pkg/coordinator/storage/errors.go pkg/coordinator/storage/put_hook_test.go
git commit -m "feat(storage): call WriteHook in Put (analyze/transform/scan/persist/on-committed)"
```

---

## Task 7: nova-image config + internal/config Image section

**Files:**
- Create: `nova-image/config.go`
- Modify: `internal/config/types.go`, `internal/config/operator_yaml.go`
- Test: `nova-image/config_test.go`

- [ ] **Step 1: Write the failing test** for defaults + validation.

```go
package novaimage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigDefaults(t *testing.T) {
	c := DefaultConfig()
	require.Contains(t, c.AllowedOutputFormats, "webp")
	require.NotContains(t, c.AllowedOutputFormats, "avif", "avif off by default")
	require.NotContains(t, c.AllowedOutputFormats, "jxl", "jxl off by default")
	require.Equal(t, 100, c.MaxMegapixels)
	require.Positive(t, c.MaxConcurrentTransforms)
	require.Positive(t, c.VipsCacheMaxMemBytes)
}

func TestConfigValidateRejectsUnknownPresetFormat(t *testing.T) {
	c := DefaultConfig()
	c.Presets = map[string]Preset{"bad": {Width: 100, Format: "avif"}} // avif not in allowed_output
	require.Error(t, c.Validate())
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test ./nova-image/ -run Config -v`

- [ ] **Step 3: Implement `nova-image/config.go`.**

```go
// Package novaimage is the nova-image product layer.
package novaimage

import "fmt"

type Preset struct {
	Width  int    `yaml:"width,omitempty"`
	Box    string `yaml:"box,omitempty"` // "WxH"
	Fit    string `yaml:"fit,omitempty"` // "cover" (box) | "" 
	Format string `yaml:"format"`
}

type FormatConversion struct {
	Enabled  bool   `yaml:"enabled"`
	Target   string `yaml:"target"`
	Lossless bool   `yaml:"lossless"`
}

type Config struct {
	AllowedInputFormats   []string          `yaml:"allowed_input_formats"`
	AllowedOutputFormats  []string          `yaml:"allowed_output_formats"`
	AllowedWidths         []int             `yaml:"allowed_widths"`
	AllowedBoxes          []string          `yaml:"allowed_boxes"`
	Presets               map[string]Preset `yaml:"presets"`
	PrewarmPresets        []string          `yaml:"prewarm_presets"`
	FormatConversion      FormatConversion  `yaml:"format_conversion"`
	MaxMegapixels         int               `yaml:"max_megapixels"`
	MaxConcurrentTransforms int             `yaml:"max_concurrent_transforms"`
	VipsCacheMaxMemBytes  int64             `yaml:"vips_cache_max_mem_bytes"`
}

func DefaultConfig() Config {
	return Config{
		AllowedInputFormats:  []string{"jpeg", "png", "webp", "gif", "tiff", "bmp"},
		AllowedOutputFormats: []string{"jpeg", "png", "webp"},
		AllowedWidths:        []int{320, 512, 1024, 2048},
		AllowedBoxes:         []string{"256x256", "1200x630"},
		Presets: map[string]Preset{
			"thumb": {Width: 256, Format: "webp"},
			"og":    {Box: "1200x630", Fit: "cover", Format: "jpeg"},
			"hero":  {Width: 1920, Format: "webp"},
		},
		PrewarmPresets:          []string{"thumb", "og"},
		FormatConversion:        FormatConversion{Enabled: false, Target: "webp", Lossless: true},
		MaxMegapixels:           100,
		MaxConcurrentTransforms: 4,
		VipsCacheMaxMemBytes:    134217728,
	}
}

func (c Config) outputAllowed(f string) bool {
	for _, a := range c.AllowedOutputFormats {
		if a == f {
			return true
		}
	}
	return false
}

func (c Config) Validate() error {
	for name, p := range c.Presets {
		if !c.outputAllowed(p.Format) {
			return fmt.Errorf("nova-image: preset %q uses output format %q not in allowed_output_formats", name, p.Format)
		}
		if p.Width == 0 && p.Box == "" {
			return fmt.Errorf("nova-image: preset %q has neither width nor box", name)
		}
	}
	if c.FormatConversion.Enabled && !c.outputAllowed(c.FormatConversion.Target) {
		return fmt.Errorf("nova-image: format_conversion.target %q not in allowed_output_formats", c.FormatConversion.Target)
	}
	return nil
}
```

- [ ] **Step 4: Add to `internal/config`.** In `types.go`, add `Image Image \`yaml:"image"\`` to `Config` and an `Image` struct mirroring the YAML (or store as `map[string]any` if you prefer the product to own parsing — simplest: the coordinator passes the raw `config.Image` to `novaimage.New`). Add defaults in `operator_yaml.go` (`applyImageDefaults`). Keep field names aligned with `nova-image/config.go`.

- [ ] **Step 5: Run — expect PASS + config loader tests.**

Run: `go test ./nova-image/ ./internal/config/ -v`

- [ ] **Step 6: Commit.**

```bash
gofmt -w nova-image/config.go nova-image/config_test.go internal/config/types.go internal/config/operator_yaml.go
git add nova-image/config.go nova-image/config_test.go internal/config/types.go internal/config/operator_yaml.go
git commit -m "feat(nova-image,config): image config (formats/presets/bounds) + defaults"
```

---

## Task 8: govips transform wrapper (libvips)

**Files:**
- Create: `nova-image/internal/transform/transform.go`, `presets.go`
- Test: `nova-image/internal/transform/transform_test.go`

- [ ] **Step 1: Write the failing tests** (resize math, encode magic bytes, megapixel reject, codec validation). Use small in-test PNGs generated via `image/png` + `bytes.Buffer`.

```go
package transform

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"
)

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{255, 0, 0, 255})
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestRenderWidthPreservesAspect(t *testing.T) {
	tr := New(Bounds{MaxMegapixels: 100, MaxConcurrent: 2})
	defer tr.Close()
	out, w, h, err := tr.Render(makePNG(t, 800, 400), Spec{Width: 200}, "webp")
	require.NoError(t, err)
	require.Equal(t, 200, w)
	require.Equal(t, 100, h)
	require.True(t, bytes.HasPrefix(out, []byte("RIFF")) && bytes.Contains(out[:16], []byte("WEBP")))
}

func TestRenderBoxCover(t *testing.T) {
	tr := New(Bounds{MaxMegapixels: 100, MaxConcurrent: 2})
	defer tr.Close()
	_, w, h, err := tr.Render(makePNG(t, 800, 400), Spec{BoxW: 100, BoxH: 100, Fit: "cover"}, "jpeg")
	require.NoError(t, err)
	require.Equal(t, 100, w)
	require.Equal(t, 100, h)
}

func TestRenderMegapixelReject(t *testing.T) {
	tr := New(Bounds{MaxMegapixels: 1, MaxConcurrent: 2}) // 1 MP cap
	defer tr.Close()
	_, _, _, err := tr.Render(makePNG(t, 2000, 2000), Spec{Width: 100}, "webp") // 4 MP source
	require.ErrorIs(t, err, ErrTooManyPixels)
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test ./nova-image/internal/transform/ -v`

- [ ] **Step 3: Implement `transform.go`.** Key elements:
  - `Startup(cacheMaxMem int64)` calls `vips.Startup(&vips.Config{MaxCacheMem: int(cacheMaxMem), ...})` once (guard with `sync.Once`); `Shutdown()` for tests.
  - `Bounds{MaxMegapixels int; MaxConcurrent int}`; `New(Bounds)` builds a `Transformer` with a `chan struct{}` semaphore of size `MaxConcurrent`.
  - `Spec{Width, BoxW, BoxH int; Fit string}`.
  - `ErrTooManyPixels`, `ErrDecode` sentinels; `ErrFormatUnsupported`.
  - `Render(src []byte, spec Spec, format string) (out []byte, w, h int, err error)`:
    1. acquire semaphore (block, with a derived timeout in the handler — here block).
    2. `vips.NewImageFromBuffer(src)`; on error → `ErrDecode`.
    3. check `img.Width()*img.Height() > MaxMegapixels*1_000_000` → `ErrTooManyPixels`.
    4. resize: width-only → `scale := float64(spec.Width)/float64(img.Width()); img.Resize(scale, vips.KernelLanczos3)`; box cover → `img.Thumbnail(spec.BoxW, spec.BoxH, vips.InterestingCentre)` (cover crop).
    5. encode by `format` via the format map (`exportParams` per format): webp `img.ExportWebp`, jpeg `ExportJpeg`, png `ExportPng`, avif/jxl via `ExportHeif`/`ExportJxl` if available.
    6. return bytes + final `img.Width()/Height()`.
  - `Decode(src []byte) (w, h int, err error)` — header decode + megapixel guard (used by AnalyzeUpload).
  - `ValidateCodecs(inputs, outputs []string) error` — for each format, `vips.NewImageFromBuffer` of a tiny sample (load) / attempt an export (save) and confirm no "unsupported" error; return a wrapped error naming the missing codec. (Used by cmd startup; tested with the known-available formats.)

- [ ] **Step 4: Implement `presets.go`** — `ResolveSpec(cfg novaimage.Config, kind, value, ext string) (Spec, presetKey string, err error)`:
  - preset → look up `cfg.Presets[value]`; not found → `ErrUnknownPreset`; key = preset name.
  - `wN` → parse N; must be in `cfg.AllowedWidths` else `ErrDimensionNotAllowed`; key = `"w"+N`.
  - `WxH` → must be in `cfg.AllowedBoxes` else `ErrDimensionNotAllowed`; key = `"WxH"`.
  - validate `ext` (normalize `jpg`→`jpeg`) ∈ `cfg.AllowedOutputFormats` else `ErrFormatNotAllowed`.

  (To avoid an import cycle, `presets.go` may take the already-extracted slices rather than `novaimage.Config`; pick whichever keeps `transform` independent of the `novaimage` package — prefer passing slices.)

- [ ] **Step 5: Run — expect PASS** (avif/jxl subtests `t.Skip` when `ValidateCodecs` reports the saver missing).

Run: `go test ./nova-image/internal/transform/ -v`

- [ ] **Step 6: Commit.**

```bash
gofmt -w nova-image/internal/transform/
git add nova-image/internal/transform/
git commit -m "feat(nova-image): govips transform wrapper (resize/encode/format-map/megapixel+concurrency bounds)"
```

---

## Task 9: image_metadata read/write

**Files:** Create `nova-image/internal/imagemeta/imagemeta.go`; Test `imagemeta_test.go`.

- [ ] **Step 1: Failing test** — `InsertMetadata` within a tx writes a row with NULL perceptual_hash; `GetMetadata` reads width/height/alt/caption. (Use `dbtest`, hand-insert a parent blob to satisfy the FK, then call the funcs.)

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement.**

```go
package imagemeta

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Insert writes the image_metadata row inside an existing tx (perceptual_hash
// NULL — Phase 1; pHash/PDQ are Phase 3/4 per the spec). alt/caption optional.
func Insert(ctx context.Context, tx pgx.Tx, cid string, w, h int, alt, caption *string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO image_metadata (cid, width, height, perceptual_hash, alt_text, caption)
		 VALUES ($1,$2,$3,NULL,$4,$5)
		 ON CONFLICT (cid) DO NOTHING`, cid, w, h, alt, caption)
	return err
}

type Meta struct {
	Width, Height  int
	AltText, Caption *string
}

func Get(ctx context.Context, pool *pgxpool.Pool, cid string) (Meta, error) {
	var m Meta
	err := pool.QueryRow(ctx,
		`SELECT width, height, alt_text, caption FROM image_metadata WHERE cid=$1`, cid).
		Scan(&m.Width, &m.Height, &m.AltText, &m.Caption)
	return m, err
}
```

- [ ] **Step 4: Run — PASS.** Run: `go test ./nova-image/internal/imagemeta/ -v`
- [ ] **Step 5: Commit.** `feat(nova-image): image_metadata read/write (perceptual_hash NULL)`

---

## Task 10: Phase-1 pass-through scanner

**Files:** Create `nova-image/internal/imagemoderation/scanner.go`; Test `scanner_test.go`.

- [ ] **Step 1: Failing test** — `Scan` returns `Action: storage.ActionAllow`.
- [ ] **Step 2: Run — FAIL.**
- [ ] **Step 3: Implement.**

```go
package imagemoderation

import (
	"context"

	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// Scanner is Phase-1 pass-through (manual moderation only). Phase 3 adds the
// Go-native pHash dedup signal; Phase 4 adds PDQ vs StopNCII/NCMEC.
type Scanner struct{}

func New() *Scanner { return &Scanner{} }

func (Scanner) Scan(ctx context.Context, plaintext []byte) storage.ScanResult {
	return storage.ScanResult{Action: storage.ActionAllow}
}
```

- [ ] **Step 4: Run — PASS.** **Step 5: Commit** `feat(nova-image): Phase-1 pass-through moderation scanner`.

---

## Task 11: /i/* handlers + single-flight find-or-create + prewarm fn

**Files:** Create `nova-image/internal/imageapi/imageapi.go`; Test `imageapi_test.go`.

This task depends on `transform` (Task 8), `imagemeta` (Task 9), and a storage surface. Define a narrow `Store` interface the handler needs (satisfied by `*storage.Service`):

```go
type Store interface {
	Resolve(ctx context.Context, cid string) (*storage.BlobView, error)
	OpenBytes(ctx context.Context, v *storage.BlobView) (io.ReadCloser, error)
	PutDerivative(ctx context.Context, plaintext []byte, dc storage.DerivativeContext,
		persist func(context.Context, pgx.Tx, string) error) (*storage.PutResult, error)
	GetDerivativeCID(ctx context.Context, parent, preset, format string) (string, bool, error)
}
```

(Add a thin `Service.GetDerivativeCID(ctx, parent, preset, format) (string, bool, error)` wrapper in `storage` that calls the gen query and maps `pgx.ErrNoRows`→`(",", false, nil)`. One-line task folded here; commit it with this task.)

- [ ] **Step 1: Write the failing test** — find-or-create + single-flight + the M5 exit criterion. Use `dbtest` + `fakeBackend` + a real `transform.Transformer` + a real `storage.Service`; seed a public collection + a parent image whose plaintext is a JPEG (use `transform` to make one, or embed a tiny JPEG). Then:
  - first `GET /i/{parent}/w512.webp` → 200, WebP magic bytes, dims w=512; a `blobs` derivative row now exists.
  - second request → served from cache (assert the derivative row count stays 1, and the backend recorded no *new* Add — track `len(fb.store)`).
  - concurrent 8× requests for a cold `/p/thumb.webp` → exactly one derivative row (single-flight).
  - negatives: non-image parent → 415; `w999.webp` (not whitelisted) → 400; `.avif` (not in default allowed_output) → 406; private parent → 401.

```go
func TestIntegrationTransformExitCriterion(t *testing.T) {
	if testing.Short() { t.Skip("integration") }
	// … wire svc + transformer + handler; seed public collection + parent JPEG via svc.Put …
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/i/"+parentCID+"/w512.webp", nil)
	router.ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	body := rr.Body.Bytes()
	require.True(t, bytes.HasPrefix(body, []byte("RIFF")))
	// derivative row exists
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE parent_cid=$1 AND derivative_preset='w512'`, parentCID).Scan(&n))
	require.Equal(t, 1, n)
}
```

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement `imageapi.go`.** Key elements:
  - `Handler{store Store, tr *transform.Transformer, cfg novaimage.Config, group singleflight.Group}` (use `golang.org/x/sync/singleflight` — add the dep; it is already an indirect dep via many libs, run `go get golang.org/x/sync/singleflight`).
  - `RegisterRoutes(r chi.Router)`: mount the six routes under `/i`.
  - A shared `serve(w, r, cidParam, kind, value, ext)`:
    1. `spec, presetKey, err := transform.ResolveSpec(allowedWidths, allowedBoxes, presets, kind, value, ext)` → map `ErrUnknownPreset`→404, `ErrDimensionNotAllowed`→400, `ErrFormatNotAllowed`→406.
    2. `view, err := store.Resolve(ctx, cid)` → map via the M3 read-error mapper (404/401/451/410/500). `view.Product != "image"` → 415.
    3. original (no transform) → `OpenBytes(view)`, set image headers (`X-Nova-Width/Height` from `imagemeta.Get`), Cache-Control per `view.Visibility`, stream.
    4. transform → `dcid, found, _ := store.GetDerivativeCID(ctx, cid, presetKey, format)`; found → `Resolve(dcid)` + `OpenBytes` + stream. miss → single-flight:
       ```go
       v, err, _ := h.group.Do(cid+"|"+presetKey+"|"+format, func() (any, error) {
           gctx := context.WithoutCancel(ctx) // cgo can't be cancelled; don't waste the work
           gctx, cancel := context.WithTimeout(gctx, 60*time.Second)
           defer cancel()
           rc, err := h.store.OpenBytes(gctx, view)            // decrypt parent
           if err != nil { return nil, err }
           defer rc.Close()
           src, _ := io.ReadAll(rc)
           out, ww, hh, err := h.tr.Render(src, spec, format)  // bounded transform
           if err != nil { return nil, err }
           persist := func(c context.Context, tx pgx.Tx, cid string) error { return imagemeta.Insert(c, tx, cid, ww, hh, nil, nil) }
           res, err := h.store.PutDerivative(gctx, out, storage.DerivativeContext{
               ParentCID: cid, Preset: presetKey, Format: format, MIME: mimeFor(format), Width: ww, Height: hh}, persist)
           if err != nil { return nil, err }
           return derivResult{cid: res.CID, bytes: out}, nil
       })
       ```
       Map `transform.ErrTooManyPixels`/`ErrDecode`→422, others→500; stream `out` with the derivative headers + Cache-Control keyed on the **parent** visibility.
  - Expose `Prewarm(ctx, parentCID string, presets []string) error` reusing the same single-flight generation per preset (best-effort: collect errors, log, return nil unless the parent is unreadable). Used by the prewarm job (Task 14).

- [ ] **Step 4: Run — PASS** (all subtests).

Run: `go test ./nova-image/internal/imageapi/ -v`

- [ ] **Step 5: Commit.**

```bash
gofmt -w nova-image/internal/imageapi/ pkg/coordinator/storage/
git add nova-image/internal/imageapi/ pkg/coordinator/storage/derivative.go go.mod go.sum
git commit -m "feat(nova-image): /i/* single-flight find-or-create + transform serve; storage.GetDerivativeCID"
```

---

## Task 12: nova-image Product implementation

**Files:** Create `nova-image/product.go`; Test `product_test.go`.

- [ ] **Step 1: Failing tests** — `Name()=="image"`; `AcceptedMimeTypes` from config; `AnalyzeUpload` on a JPEG returns ImageMetadata (w/h) + `ActionAllow` + (with conversion off) nil transformed; with `format_conversion.enabled` + a PNG, returns transformed WebP + `ResultMIME`; non-image MIME → error; `OnDelete` cascades child state.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement `product.go`.**
  - `Product{cfg Config; tr *transform.Transformer; scanner *imagemoderation.Scanner; api *imageapi.Handler; queue *jobs.Queue}` (queue used by OnCommitted).
  - `New(cfg Config, store imageapi.Store, queue *jobs.Queue) *Product` — builds the transformer (`transform.New`), scanner, and the imageapi handler.
  - `Name() string { return "image" }`.
  - `AcceptedMimeTypes() []string` — map `cfg.AllowedInputFormats` → MIME (`jpeg`→`image/jpeg`, …).
  - `Migrations() (fs.FS, string)` — return an empty `embed.FS` (image_metadata is core-owned; reconciliation #1).
  - `RegisterRoutes(r chi.Router) { p.api.RegisterRoutes(r) }`.
  - `ImageMetadata{Width, Height int; Alt, Caption *string}` implementing `product.Metadata` (`ProductName()="image"`; `Persist` = `imagemeta.Insert`).
  - `AnalyzeUpload(ctx, uc, plaintext io.Reader)`:
    ```go
    buf, _ := io.ReadAll(plaintext)
    if !p.accepts(uc.DeclaredMimeType) { return nil, nil, nil, ErrNotAnImage } // → handler 415 / hook reject
    w, h, err := p.tr.Decode(buf)          // megapixel guard inside
    if err != nil { return nil, nil, nil, err }
    scan := p.scanner.Scan(ctx, buf)
    var transformed io.Reader
    md := ImageMetadata{Width: w, Height: h}
    if p.cfg.FormatConversion.Enabled && isLosslessInput(uc.DeclaredMimeType) && !preserveOriginal(uc) {
        out, ww, hh, cerr := p.tr.Render(buf, transform.Spec{}, p.cfg.FormatConversion.Target) // no-resize re-encode
        if cerr == nil { transformed = bytes.NewReader(out); md.Width, md.Height = ww, hh }
    }
    return md, &scan, transformed, nil
    ```
    (`preserveOriginal` consults the collection policy — Phase 1 may stub to false until the policy field is surfaced; note this as a known simplification.)
  - `OnCommitted(ctx, ref, md)` — `p.queue.Enqueue(ctx, kinds.KindDerivativePrewarm, prewarmPayload{ParentCID: ref.CID, Presets: p.cfg.PrewarmPresets})` (best-effort; log on error).
  - `OnDelete(ctx, tx, parentCID, newState)` — `tx.Exec(ctx, "UPDATE blobs SET state=$1 WHERE parent_cid=$2", newState, parentCID)`. (Child DEK shred enumeration is the core tombstone's job in M9, which joins on `parent_cid`; document in a comment.)

- [ ] **Step 4: Run — PASS.** Run: `go test ./nova-image/ -v`
- [ ] **Step 5: Commit.** `feat(nova-image): Product impl (AnalyzeUpload/OnCommitted/OnDelete/RegisterRoutes/Migrations)`

---

## Task 13: coordinator.RegisterProduct + WriteHook adapter + worker pool

**Files:** Modify `pkg/coordinator/coordinator.go`; Test `coordinator_test.go`.

- [ ] **Step 1: Failing tests** — `RegisterProduct` rejects a product whose routes would collide with a reserved prefix (`/api/v1`, `/blob`, `/health`, `/fed/v1`, `/legal`); a registered product's `/i/*` routes are reachable; the worker pool runs (enqueue a `derivative_prewarm` job and observe the prewarm fn invoked).

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement.**
  - Add `jobs.Queue` + `jobs.WorkerPool` construction in `New` (queue over the pool; pool with default options).
  - `func (c *Coordinator) RegisterProduct(p product.Product)`: append to `c.products`; the prefix check runs when routes are mounted (in `NewServer`, wrap the chi router so a product mounting under a reserved prefix panics/errors at registration — simplest: validate `p.Name()`’s conventional prefix is not reserved, and document that `RegisterRoutes` must stay under `/i` etc.).
  - **Adapter** (`productHook` implementing `storage.WriteHook`), in this package:
    ```go
    type productHook struct{ p product.Product }
    func (h productHook) Analyze(ctx context.Context, pc storage.PutContext, plaintext []byte) (storage.AnalyzeResult, error) {
        uc := &product.UploadContext{DeclaredMimeType: pc.MIME /* + collection/owner mapping */}
        md, scan, transformed, err := h.p.AnalyzeUpload(ctx, uc, bytes.NewReader(plaintext))
        if err != nil { return storage.AnalyzeResult{}, err }
        ar := storage.AnalyzeResult{Scan: *scan, Persist: md.Persist}
        if transformed != nil {
            b, _ := io.ReadAll(transformed)
            ar.Transformed = b
            ar.ResultMIME = "image/" + /* derive from product/config */ ""
        }
        return ar, nil
    }
    func (h productHook) OnCommitted(ctx context.Context, ref storage.CommittedRef) {
        _ = h.p.OnCommitted(ctx, &ref, nil) // metadata already persisted in-tx; ref carries the CID
    }
    ```
    Wire `storage.NewService(..., storage.WithProductHook(productHook{p}))` when a product is registered. (Since `New` currently builds the Service before products register, restructure so `RegisterProduct` is called before `New` finalizes, or build the Service lazily in `Run`. Cleanest: accept products in `New` via the `Config`/options, or add `RegisterProduct` that must be called before `Run` and have `Run` finalize the Service+server. Match the spec's "register before Run" contract from `PRODUCT_MODULE_INTERFACE.md`.)
  - In `Run`: `pool.RegisterHandler(kinds.KindDerivativePrewarm, kinds.NewDerivativePrewarmHandler(prewarmFn)); go pool.Run(ctx)` where `prewarmFn` calls the registered image product's `Prewarm`.

- [ ] **Step 4: Run — PASS.** Run: `go test ./pkg/coordinator/ -v`
- [ ] **Step 5: Commit.** `feat(coordinator): RegisterProduct + product→WriteHook adapter + worker pool startup`

---

## Task 14: derivative_prewarm handler body

**Files:** Modify `internal/jobs/kinds/derivative_prewarm.go`; Test `derivative_prewarm_test.go`.

- [ ] **Step 1: Failing test** — `NewDerivativePrewarmHandler(fn)` decodes the payload and calls `fn(ctx, parent, presets)`; returns the fn's error (so the queue retries); unknown/garbage payload → error.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement.**

```go
package kinds

import (
	"context"
	"encoding/json"

	"github.com/nova-archive/nova/internal/jobs"
)

const KindDerivativePrewarm = "derivative_prewarm"

type DerivativePrewarmPayload struct {
	ParentCID string   `json:"parent_cid"`
	Presets   []string `json:"presets"`
}

// NewDerivativePrewarmHandler builds the handler that pre-generates presets for
// a freshly-committed image. prewarm is the nova-image generation fn (best-effort:
// it should log per-preset failures and return an error only when the parent is
// unreadable, so the queue's backoff retries a transient failure).
func NewDerivativePrewarmHandler(prewarm func(ctx context.Context, parentCID string, presets []string) error) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p DerivativePrewarmPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return err
		}
		return prewarm(ctx, p.ParentCID, p.Presets)
	}
}
```

- [ ] **Step 4: Run — PASS.** Run: `go test ./internal/jobs/kinds/ -v`
- [ ] **Step 5: Commit.** `feat(jobs): real derivative_prewarm handler (find-or-create per preset)`

---

## Task 15: /api/v1/images route + handler

**Files:** Modify `internal/api/handlers/upload.go`, `internal/api/server.go`; Test `upload_test.go`.

- [ ] **Step 1: Failing test** — `POST /api/v1/images` with a JPEG part + `collection_id` → 201 `UploadResult` with `product:"image"` and `urls.presets` populated; a non-image (`text/plain`) part → 415.

- [ ] **Step 2: Run — FAIL.**

- [ ] **Step 3: Implement.** Add `ImageAccepts func(mime string) bool` and `PrewarmPresetURLs func(cid string) map[string]string` to `ServerConfig` (nil unless nova-image registered). Add `MultipartImage` handler = `Multipart` with `product` forced to `"image"`, an early `if h.imageAccepts != nil && !h.imageAccepts(sniffOrDeclared) { 415 }`, and `writeUploadResult` extended to include `urls.presets` when `product=="image"`. Mount `r.Post("/api/v1/images", cfg.Upload.MultipartImage)` next to `/api/v1/blobs`. Map `storage.ErrModerationRejected`→422 in `writePutError`.

- [ ] **Step 4: Run — PASS.** Run: `go test ./internal/api/... -v`
- [ ] **Step 5: Commit.** `feat(api): /api/v1/images (force product=image, early 415, presets in result)`

---

## Task 16: cmd/coordinator wiring + vips.Startup + codec validation

**Files:** Modify `cmd/coordinator/main.go`. Test: covered by `transform.ValidateCodecs` unit test (Task 8) + build.

- [ ] **Step 1:** Load `cfg.Image`; call `transform.Startup(cfg.Image.VipsCacheMaxMemBytes)`; `if err := transform.ValidateCodecs(cfg.Image.AllowedInputFormats, cfg.Image.AllowedOutputFormats); err != nil { log.Fatal(refuse-to-start, naming the codec) }`; build the image product `novaimage.New(cfg.Image, svc, queue)`; `coord.RegisterProduct(img)` **before** `coord.Run(ctx)`; log a destructive-conversion notice when `cfg.Image.FormatConversion.Enabled && !Lossless`.
- [ ] **Step 2: Build + vet.** Run: `go build ./cmd/coordinator/ && go vet ./cmd/coordinator/`
- [ ] **Step 3: Commit.** `feat(coordinator): register nova-image; vips startup + codec refuse-to-start`

---

## Task 17: dev nginx + integration test + CI libvips

**Files:** Modify `docker/nginx/nova.dev.conf`; Create `internal/integration/m5_image_test.go`; Modify `.github/workflows/ci.yml`.

- [ ] **Step 1:** Extend `nova.dev.conf` to `proxy_pass` `/i/` and `/api/v1/images` to the coordinator (mirror the M3/M4 location blocks).
- [ ] **Step 2: Write the integration test** (model on `m4_upload_test.go`): coordinator in-process + real `EmbeddedBackend` (offline) + `Keystore`, fronted by dev nginx; seed a public collection; upload a JPEG via `/api/v1/images` and via tus; `GET /i/{cid}/w512.webp` through nginx → 200 + 512-px WebP; `/i/{cid}/p/thumb.webp` → 200; second request served from the persisted derivative (one row); poll until prewarm presets exist; negatives (415/400/406/401). Guard with `testing.Short()`; record the nginx-flaky fallback (target coordinator directly) as M3/M4 do.
- [ ] **Step 3:** Add to `ci.yml` `test` + `image-build` jobs: install libvips, e.g. (Ubuntu runner) `sudo apt-get update && sudo apt-get install -y libvips-dev libheif-dev libjxl-dev`. Confirm `CGO_ENABLED=1`.
- [ ] **Step 4: Run.** Run: `go test ./internal/integration/ -run M5 -v` (with libvips present). Expected: PASS.
- [ ] **Step 5: Commit.** `test(m5): nginx-fronted transform + prewarm round-trip; CI libvips; dev nginx /i/* + /api/v1/images`

---

## Task 18: Documentation reconciliations (the six from the spec)

**Files:** `docs/specs/openapi.yaml`, `docs/specs/PRODUCT_MODULE_INTERFACE.md`, `docs/specs/DATA_MODEL.sql`, `internal/db/migrations/0001_init.sql`, `docs/ROADMAP.md`, `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md`, `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md`.

- [ ] **Step 1:** `openapi.yaml` — add `jxl` to the `ext` enums on `/i/{cid}.{ext}`, `/i/{cid}/{w}x{h}.{ext}`, `/i/{cid}/w{w}.{ext}` (note operator-gated/off-by-default); add `406` (output format not enabled) and `400` (dimension not whitelisted) responses to the transform routes.
- [ ] **Step 2:** `PRODUCT_MODULE_INTERFACE.md` — § "Database conventions": note `image_metadata` is core-owned (in `DATA_MODEL.sql`); the products-own-migrations rule governs *future new* tables. § "AnalyzeUpload" + "Reference: nova-image": replace the monolithic "PDQ" with the two-track plan (Go-native pHash dedup → Phase 3; PDQ vs StopNCII/NCMEC → Phase 4).
- [ ] **Step 3:** `DATA_MODEL.sql` + `0001_init.sql` — change the `perceptual_hash` comment to: `32 bytes; Phase 3 Go-native 256-bit pHash (dedup), Phase 4 PDQ (external matching); NULL until then` (comment only — no schema change; do not alter any SQL statement).
- [ ] **Step 4:** `ROADMAP.md` — Phase 3 line: name the Go-native pHash for near-dup; Phase 4 line: name PDQ for StopNCII/NCMEC.
- [ ] **Step 5:** MVP design — resolve open question #1 + the base-image decision to Debian-slim/glibc; note worker-pool startup landed in M5. Master plan — mark M5 in progress; link this plan.
- [ ] **Step 6: Verify nothing else references the removed `ResolveBlobVisibility`.** Run: `grep -rn "ResolveBlobVisibility" --include=*.go .` Expected: no matches. Then `go build ./... && go test ./... -short`.
- [ ] **Step 7: Commit.** `docs(m5): apply reconciliations (PDQ two-track, image_metadata core-owned, jxl/codes, base image)`

---

## Final verification (before merge)

- [ ] `go build ./...` clean.
- [ ] `go test ./... -short` (unit) green; `go test ./...` (integration, libvips + docker present) green.
- [ ] `make codegen-check` clean; `go vet ./...` clean; `gofmt -l` lists no files you created.
- [ ] The M5 exit criterion passes end-to-end: a curl upload of a JPEG then `GET /i/{cid}/w512.webp` returns a 512-px WebP through nginx.
- [ ] Then: `superpowers:finishing-a-development-branch` → fast-forward merge `m5-image-transforms` to `main` + annotated tag `m5-image-transforms` (local; no push), per the milestone workflow.

---

## Self-review (against the spec)

- **Spec coverage:** Product interface (T1), WriteHook seam (T2,T6), PutDerivative + dedup/loser-unpin (T5), parent-aware visibility (T3), transform wrapper + bounds (T8), formats/config incl. AVIF/JXL-off-by-default + format-conversion (T7,T8,T12), `/i/*` + single-flight + detached-ctx (T11), `/api/v1/images` (T15), prewarm + worker pool (T13,T14), OnDelete cascade (T12), startup codec validation + transcoding-fidelity notice (T16), resource bounds (T7,T8,T11), Debian-slim/CI libvips (T17), six reconciliations (T18). All spec sections map to a task.
- **Placeholder scan:** the two acknowledged simplifications (`preserveOriginal` stub pending the collection-policy field; `ResultMIME` derivation in the adapter) are explicitly flagged with how to resolve them — not silent TODOs.
- **Type consistency:** `storage.WriteHook`/`AnalyzeResult`/`CommittedRef`/`ScanResult`/`ScanAction`/`ActionAllow`, `DerivativeContext`, `product.Product`/`Metadata`/`UploadContext`, `transform.Spec`/`Bounds`/`ErrTooManyPixels`, `kinds.KindDerivativePrewarm`/`DerivativePrewarmPayload`/`NewDerivativePrewarmHandler`, and the `ResolveEffectiveVisibility`/`InsertDerivativeBlob`/`GetDerivativeCID` query names are used consistently across tasks. sqlc-emitted Go param/field names (nullable `text` → `*string`) are flagged to confirm-after-regen in T4/T5.
