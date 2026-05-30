package imageapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	novaimage "github.com/nova-archive/nova/nova-image"
	"github.com/nova-archive/nova/nova-image/internal/imagemeta"
	"github.com/nova-archive/nova/nova-image/internal/transform"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"golang.org/x/sync/singleflight"
)

// Handler serves the /i/* image content + transform routes.
type Handler struct {
	store Store
	tr    *transform.Transformer
	cfg   novaimage.Config
	pool  *pgxpool.Pool
	group singleflight.Group
}

// New builds a Handler.
func New(store Store, tr *transform.Transformer, cfg novaimage.Config, pool *pgxpool.Pool) *Handler {
	return &Handler{store: store, tr: tr, cfg: cfg, pool: pool}
}

// RegisterRoutes mounts the /i/* routes on r. The static "p" segment
// outranks the {xform} param so /i/{cid}/p/{preset} wins.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/i/{seg}", h.serveTop)
	r.Get("/i/{cid}/p/{preset}", h.servePreset)
	r.Get("/i/{cid}/{xform}", h.serveXform)
}

// ---------------------------------------------------------------------------
// Helper: MIME, cache-control, error mapping.
// ---------------------------------------------------------------------------

func mimeFor(format string) string {
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	case "avif":
		return "image/avif"
	case "jxl":
		return "image/jxl"
	case "gif":
		return "image/gif"
	}
	return "application/octet-stream"
}

func cacheControl(v storage.Visibility) string {
	if v == storage.VisibilityPublic {
		return "public, max-age=31536000, immutable"
	}
	return "private, max-age=300, must-revalidate"
}

func mapResolveErr(err error) int {
	switch {
	case errors.Is(err, storage.ErrBlobNotFound):
		return http.StatusNotFound
	case errors.Is(err, storage.ErrBlobAuthRequired):
		return http.StatusUnauthorized
	case errors.Is(err, storage.ErrBlobQuarantined):
		return http.StatusUnavailableForLegalReasons
	case errors.Is(err, storage.ErrBlobSoftDeleted),
		errors.Is(err, storage.ErrBlobTombstoned),
		errors.Is(err, storage.ErrKeyShredded):
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}

func mapSpecErr(err error) int {
	switch {
	case errors.Is(err, ErrUnknownPreset):
		return http.StatusNotFound
	case errors.Is(err, ErrDimensionNotAllowed):
		return http.StatusBadRequest
	case errors.Is(err, ErrFormatNotAllowed):
		return http.StatusNotAcceptable
	default:
		return http.StatusBadRequest
	}
}

// transformStatus maps generation errors to HTTP status.
// Decode/megapixel failures → 422; everything else → 500.
func transformStatus(err error) int {
	if errors.Is(err, transform.ErrTooManyPixels) || errors.Is(err, transform.ErrDecode) {
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

// splitExt splits "base.ext" → ("base", "ext"). Returns ("s","") if no dot.
func splitExt(s string) (string, string) {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// ---------------------------------------------------------------------------
// Route-resolution helpers.
// ---------------------------------------------------------------------------

// resolveImageParent resolves + authorises the parent CID and confirms it is
// an image blob. Writes the error status and returns nil on failure.
func (h *Handler) resolveImageParent(w http.ResponseWriter, r *http.Request, cid string) *storage.BlobView {
	v, err := h.store.Resolve(r.Context(), cid)
	if err != nil {
		w.WriteHeader(mapResolveErr(err))
		return nil
	}
	if v.Product != "image" {
		w.WriteHeader(http.StatusUnsupportedMediaType) // 415
		return nil
	}
	return v
}

// ---------------------------------------------------------------------------
// Route handlers.
// ---------------------------------------------------------------------------

// serveTop handles /i/{seg}: bare cid, {cid}.json, or {cid}.{ext} transcode.
func (h *Handler) serveTop(w http.ResponseWriter, r *http.Request) {
	seg := chi.URLParam(r, "seg")
	if strings.HasSuffix(seg, ".json") {
		h.serveJSON(w, r, strings.TrimSuffix(seg, ".json"))
		return
	}
	if i := strings.LastIndexByte(seg, '.'); i >= 0 {
		h.transformAndServe(w, r, seg[:i], KindOrig, "", seg[i+1:])
		return
	}
	h.serveOriginal(w, r, seg)
}

func (h *Handler) servePreset(w http.ResponseWriter, r *http.Request) {
	cid := chi.URLParam(r, "cid")
	preset := chi.URLParam(r, "preset")
	name, ext := splitExt(preset)
	if ext == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	h.transformAndServe(w, r, cid, KindPreset, name, ext)
}

func (h *Handler) serveXform(w http.ResponseWriter, r *http.Request) {
	cid := chi.URLParam(r, "cid")
	xform := chi.URLParam(r, "xform")
	base, ext := splitExt(xform)
	if ext == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if strings.HasPrefix(base, "w") && !strings.ContainsRune(base, 'x') {
		h.transformAndServe(w, r, cid, KindWidth, strings.TrimPrefix(base, "w"), ext)
		return
	}
	h.transformAndServe(w, r, cid, KindBox, base, ext)
}

// ---------------------------------------------------------------------------
// Core: find-or-create derivative.
// ---------------------------------------------------------------------------

// genResult is the output of a successful generate call.
type genResult struct {
	bytes []byte
	cid   string
}

// generate decodes the parent, applies spec, stores the derivative, and
// returns the result. It is called inside the singleflight group and from
// Prewarm. ctx should be detached (not cancellable by client disconnect).
func (h *Handler) generate(ctx context.Context, pv *storage.BlobView, parentCID string, spec transform.Spec, presetKey, format string) (genResult, error) {
	rc, err := h.store.OpenBytes(ctx, pv)
	if err != nil {
		return genResult{}, err
	}
	src, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return genResult{}, err
	}

	out, ww, hh, err := h.tr.Render(src, spec, format)
	if err != nil {
		return genResult{}, err
	}

	persist := func(c context.Context, tx pgx.Tx, dcid string) error {
		return imagemeta.Insert(c, tx, dcid, ww, hh, nil, nil)
	}

	pr, err := h.store.PutDerivative(ctx, out, storage.DerivativeContext{
		ParentCID: parentCID,
		Preset:    presetKey,
		Format:    format,
		MIME:      mimeFor(format),
		Width:     ww,
		Height:    hh,
	}, persist)
	if err != nil {
		return genResult{}, err
	}
	return genResult{bytes: out, cid: pr.CID}, nil
}

// transformAndServe is the find-or-create core for all transform routes.
func (h *Handler) transformAndServe(w http.ResponseWriter, r *http.Request, cid string, kind Kind, value, ext string) {
	spec, presetKey, format, err := ResolveSpec(h.cfg, kind, value, ext)
	if err != nil {
		w.WriteHeader(mapSpecErr(err))
		return
	}

	pv := h.resolveImageParent(w, r, cid)
	if pv == nil {
		return
	}

	// Cache hit?
	if dcid, found, gerr := h.store.GetDerivativeCID(r.Context(), cid, presetKey, format); gerr == nil && found {
		dv, derr := h.store.Resolve(r.Context(), dcid)
		if derr == nil {
			rc, oerr := h.store.OpenBytes(r.Context(), dv)
			if oerr == nil {
				defer rc.Close()
				body, _ := io.ReadAll(rc)
				h.writeImage(w, body, mimeFor(format), pv.Visibility, dcid)
				return
			}
		}
		// fall through to regenerate on any cache-hit path error
	}

	// Miss: single-flight generation. Use a detached context so that a client
	// disconnect does not cancel cgo work or an in-progress DB commit.
	key := cid + "|" + presetKey + "|" + format
	res, err, _ := h.group.Do(key, func() (any, error) {
		gctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
		defer cancel()
		return h.generate(gctx, pv, cid, spec, presetKey, format)
	})
	if err != nil {
		w.WriteHeader(transformStatus(err))
		return
	}
	g := res.(genResult)
	h.writeImage(w, g.bytes, mimeFor(format), pv.Visibility, g.cid)
}

// ---------------------------------------------------------------------------
// Response writers.
// ---------------------------------------------------------------------------

func (h *Handler) writeImage(w http.ResponseWriter, body []byte, mime string, vis storage.Visibility, cid string) {
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", cacheControl(vis))
	w.Header().Set("X-Nova-Cid", cid)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// serveOriginal serves the stored original bytes (no transform).
func (h *Handler) serveOriginal(w http.ResponseWriter, r *http.Request, cid string) {
	pv := h.resolveImageParent(w, r, cid)
	if pv == nil {
		return
	}
	rc, err := h.store.OpenBytes(r.Context(), pv)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)

	w.Header().Set("Content-Type", pv.MIME)
	w.Header().Set("Cache-Control", cacheControl(pv.Visibility))
	w.Header().Set("X-Nova-Cid", pv.CID)
	if m, merr := imagemeta.Get(r.Context(), h.pool, cid); merr == nil {
		w.Header().Set("X-Nova-Width", strconv.Itoa(m.Width))
		w.Header().Set("X-Nova-Height", strconv.Itoa(m.Height))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// serveJSON serves /i/{cid}.json — public image metadata.
func (h *Handler) serveJSON(w http.ResponseWriter, r *http.Request, cid string) {
	pv := h.resolveImageParent(w, r, cid)
	if pv == nil {
		return
	}
	m, err := imagemeta.Get(r.Context(), h.pool, cid)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", cacheControl(pv.Visibility))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"cid":       pv.CID,
		"mime_type": pv.MIME,
		"width":     m.Width,
		"height":    m.Height,
		"alt_text":  m.AltText,
		"caption":   m.Caption,
	})
}

// ---------------------------------------------------------------------------
// Prewarm.
// ---------------------------------------------------------------------------

// Prewarm generates the named presets for a freshly-committed parent.
// Best-effort: per-preset failures are joined and returned, but a missing
// or unreadable parent is the only hard error.
func (h *Handler) Prewarm(ctx context.Context, parentCID string, presets []string) error {
	pv, err := h.store.Resolve(ctx, parentCID)
	if err != nil {
		return err
	}
	if pv.Product != "image" {
		return nil
	}

	var errs []error
	for _, name := range presets {
		p, ok := h.cfg.Presets[name]
		if !ok {
			continue
		}
		// Use the preset's own format as the ext so ResolveSpec validates it.
		spec, presetKey, format, serr := ResolveSpec(h.cfg, KindPreset, name, p.Format)
		if serr != nil {
			errs = append(errs, serr)
			continue
		}
		if _, found, _ := h.store.GetDerivativeCID(ctx, parentCID, presetKey, format); found {
			continue
		}
		if _, gerr := h.generate(ctx, pv, parentCID, spec, presetKey, format); gerr != nil {
			errs = append(errs, gerr)
		}
	}
	return errors.Join(errs...)
}
