// Package imageproduct wires the nova-image product: it implements
// pkg/coordinator/product.Product, composing the transform layer, the
// moderation scanner, the image_metadata side-table writer, and the /i/* read
// handler. It is deliberately a non-internal package so cmd/coordinator can
// construct it, while still being able to import nova-image/internal/*.
package imageproduct

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	novaimage "github.com/nova-archive/nova/nova-image"
	"github.com/nova-archive/nova/nova-image/internal/imageapi"
	"github.com/nova-archive/nova/nova-image/internal/imagemeta"
	"github.com/nova-archive/nova/nova-image/internal/imagemoderation"
	"github.com/nova-archive/nova/nova-image/internal/transform"
	"github.com/nova-archive/nova/internal/jobs"
	"github.com/nova-archive/nova/internal/jobs/kinds"
	"github.com/nova-archive/nova/pkg/coordinator/product"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// ErrNotAnImage rejects an upload whose declared MIME is not an enabled input
// format. The coordinator maps this to a 415 at the upload edge.
var ErrNotAnImage = fmt.Errorf("nova-image: declared mime is not an accepted image type")

// formatMIME maps a config format token to its canonical MIME type.
var formatMIME = map[string]string{
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"webp": "image/webp",
	"gif":  "image/gif",
	"tiff": "image/tiff",
	"bmp":  "image/bmp",
	"avif": "image/avif",
	"jxl":  "image/jxl",
}

// Product is the nova-image product singleton.
type Product struct {
	cfg     novaimage.Config
	tr      *transform.Transformer
	scanner *imagemoderation.Scanner
	api     *imageapi.Handler
	queue   *jobs.Queue
}

// New builds the product: the transformer (bounded by config), the scanner, and
// the /i/* handler. store/pool/queue may be nil in unit tests that exercise only
// AnalyzeUpload/OnDelete/metadata; OnCommitted no-ops when queue is nil.
func New(cfg novaimage.Config, store imageapi.Store, pool *pgxpool.Pool, queue *jobs.Queue) *Product {
	tr := transform.New(transform.Bounds{
		MaxMegapixels: cfg.MaxMegapixels,
		MaxConcurrent: cfg.MaxConcurrentTransforms,
	})
	return &Product{
		cfg:     cfg,
		tr:      tr,
		scanner: imagemoderation.New(),
		api:     imageapi.New(store, tr, cfg, pool),
		queue:   queue,
	}
}

func (p *Product) Name() string { return "image" }

// AcceptedMimeTypes returns the MIME types derived from the operator's enabled
// input formats.
func (p *Product) AcceptedMimeTypes() []string {
	out := make([]string, 0, len(p.cfg.AllowedInputFormats))
	for _, f := range p.cfg.AllowedInputFormats {
		if m, ok := formatMIME[f]; ok {
			out = append(out, m)
		}
	}
	return out
}

// accepts reports whether mime maps to an enabled input format.
func (p *Product) accepts(mime string) bool {
	for _, f := range p.cfg.AllowedInputFormats {
		if formatMIME[f] == mime {
			return true
		}
	}
	return false
}

// Migrations: image_metadata is core-owned (it lives in DATA_MODEL.sql /
// 0001_init.sql), so the product ships no migrations of its own in Phase 1.
// The products-own-migrations rule governs future NEW tables.
func (p *Product) Migrations() (fs.FS, string) { return embed.FS{}, "" }

func (p *Product) RegisterRoutes(r chi.Router) { p.api.RegisterRoutes(r) }

// Prewarm pre-generates the named presets for a committed parent. Delegates to
// the handler's find-or-create path; used by the derivative_prewarm worker.
func (p *Product) Prewarm(ctx context.Context, parentCID string, presets []string) error {
	return p.api.Prewarm(ctx, parentCID, presets)
}

// AnalyzeUpload validates the declared type, decodes for dimensions (megapixel
// guard inside Decode), scans, and — when format conversion is enabled for a
// lossless input — re-encodes to the target format. The re-encode honors
// FormatConversion.Lossless so a "lossless" conversion does not degrade the
// canonical stored original. plaintext is consumed fully here and not retained.
func (p *Product) AnalyzeUpload(ctx context.Context, uc *product.UploadContext, plaintext io.Reader) (
	product.Metadata, *storage.ScanResult, io.Reader, error) {

	buf, err := io.ReadAll(plaintext)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("nova-image: read upload: %w", err)
	}
	if !p.accepts(uc.DeclaredMimeType) {
		return nil, nil, nil, ErrNotAnImage
	}
	w, h, err := p.tr.Decode(buf)
	if err != nil {
		return nil, nil, nil, err
	}
	scan := p.scanner.Scan(ctx, buf)

	md := ImageMetadata{Width: w, Height: h}
	var transformed io.Reader
	if p.cfg.FormatConversion.Enabled && isLosslessInput(uc.DeclaredMimeType) && !preserveOriginal(uc) {
		out, ww, hh, cerr := p.tr.Render(buf, transform.Spec{Lossless: p.cfg.FormatConversion.Lossless}, p.cfg.FormatConversion.Target)
		if cerr == nil {
			transformed = bytes.NewReader(out)
			md.Width, md.Height = ww, hh
		}
		// On a conversion error we fall back to storing the original (transformed stays nil).
	}
	return md, &scan, transformed, nil
}

// OnCommitted enqueues a best-effort prewarm of the configured presets. The blob
// is already committed; a returned error is logged by the caller, not fatal.
func (p *Product) OnCommitted(ctx context.Context, blob *storage.CommittedRef, _ product.Metadata) error {
	if p.queue == nil {
		return nil
	}
	payload, err := json.Marshal(kinds.DerivativePrewarmPayload{
		ParentCID: blob.CID,
		Presets:   p.cfg.PrewarmPresets,
	})
	if err != nil {
		return fmt.Errorf("nova-image: marshal prewarm payload: %w", err)
	}
	if _, err := p.queue.Enqueue(ctx, kinds.KindDerivativePrewarm, payload); err != nil {
		return fmt.Errorf("nova-image: enqueue prewarm: %w", err)
	}
	return nil
}

// OnDelete cascades the parent's new lifecycle state to its derivatives. Child
// DEK shredding on tombstone is the core's job in M9 (it enumerates blobs by
// parent_cid); here we only propagate the state column.
func (p *Product) OnDelete(ctx context.Context, tx pgx.Tx, parentCID, newState string) error {
	_, err := tx.Exec(ctx, `UPDATE blobs SET state = $1 WHERE parent_cid = $2`, newState, parentCID)
	return err
}

// ImageMetadata is the product.Metadata for an image upload; Persist writes the
// core-owned image_metadata row inside the storage write transaction.
type ImageMetadata struct {
	Width, Height int
	Alt, Caption  *string
}

func (m ImageMetadata) ProductName() string { return "image" }

func (m ImageMetadata) Persist(ctx context.Context, tx pgx.Tx, cid string) error {
	return imagemeta.Insert(ctx, tx, cid, m.Width, m.Height, m.Alt, m.Caption)
}

// isLosslessInput reports whether a re-encode of mime would be a destructive
// step worth avoiding — i.e. the input is itself a lossless format (PNG/BMP/TIFF).
func isLosslessInput(mime string) bool {
	switch mime {
	case "image/png", "image/bmp", "image/tiff":
		return true
	}
	return false
}

// preserveOriginal reports whether the collection policy forbids upload-time
// format conversion. Phase-1 stub: the per-collection policy field is not yet
// surfaced through UploadContext, so we never preserve. KNOWN SIMPLIFICATION —
// consult uc here once the policy lands (post-M5).
func preserveOriginal(uc *product.UploadContext) bool { return false }

// Compile-time interface checks.
var (
	_ product.Product  = (*Product)(nil)
	_ product.Metadata = ImageMetadata{}
)
