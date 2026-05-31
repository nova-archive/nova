package coordinator

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/nova-archive/nova/pkg/coordinator/product"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// productHook adapts the registered products to the storage.WriteHook seam, so
// the storage core can run product analysis/transform without importing the
// product package (the dependency inversion the seam exists for). It dispatches
// by PutContext.Product; an unknown product is an allow-passthrough — that is
// the M4 raw write path, where no product owns the blob.
type productHook struct {
	products map[string]product.Product
}

func (h *productHook) Analyze(ctx context.Context, pc storage.PutContext, plaintext []byte) (storage.AnalyzeResult, error) {
	p, ok := h.products[pc.Product]
	if !ok {
		return storage.AnalyzeResult{Scan: storage.ScanResult{Action: storage.ActionAllow}}, nil
	}
	uc := &product.UploadContext{DeclaredMimeType: pc.MIME}
	md, scan, transformed, err := p.AnalyzeUpload(ctx, uc, bytes.NewReader(plaintext))
	if err != nil {
		return storage.AnalyzeResult{}, err
	}
	ar := storage.AnalyzeResult{Scan: storage.ScanResult{Action: storage.ActionAllow}}
	if scan != nil {
		ar.Scan = *scan
	}
	if md != nil {
		ar.Persist = md.Persist
	}
	if transformed != nil {
		b, rerr := io.ReadAll(transformed)
		if rerr != nil {
			return storage.AnalyzeResult{}, rerr
		}
		ar.Transformed = b
		// ResultMIME via content sniffing. Accurate for the Phase-1 conversion
		// targets (webp/jpeg/png); an avif/jxl target would need the product to
		// surface the chosen format explicitly (known limitation, documented).
		ar.ResultMIME = http.DetectContentType(b)
	}
	return ar, nil
}

func (h *productHook) OnCommitted(ctx context.Context, ref storage.CommittedRef) {
	if p, ok := h.products[ref.Product]; ok {
		_ = p.OnCommitted(ctx, &ref, nil)
	}
}

var _ storage.WriteHook = (*productHook)(nil)
