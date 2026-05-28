package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// Reader is the storage read surface the blob handlers depend on.
type Reader interface {
	Resolve(ctx context.Context, cid string) (*storage.BlobView, error)
	OpenBytes(ctx context.Context, v *storage.BlobView) (io.ReadCloser, error)
}

// BlobHandler serves /blob/{cid} (GET, HEAD) and /blob/{cid}.json.
type BlobHandler struct{ r Reader }

// NewBlobHandler builds a blob handler over a storage reader.
func NewBlobHandler(r Reader) *BlobHandler { return &BlobHandler{r: r} }

func cacheControl(v storage.Visibility) string {
	if v == storage.VisibilityPublic {
		return "public, max-age=31536000, immutable"
	}
	return "private, max-age=300, must-revalidate"
}

func mapBytesError(err error) (int, string, string) {
	switch {
	case errors.Is(err, storage.ErrBlobNotFound):
		return 404, "not_found", "blob not found"
	case errors.Is(err, storage.ErrBlobAuthRequired):
		return 401, "signed_url_required", "signed url or bearer required"
	case errors.Is(err, storage.ErrBlobQuarantined):
		return 451, "quarantined", "content under moderation hold"
	case errors.Is(err, storage.ErrBlobSoftDeleted),
		errors.Is(err, storage.ErrBlobTombstoned),
		errors.Is(err, storage.ErrKeyShredded):
		return 410, "gone", "content no longer available"
	default:
		return 500, "internal", "internal server error"
	}
}

func mapJSONError(err error) (int, string, string) {
	switch {
	case errors.Is(err, storage.ErrBlobNotFound),
		errors.Is(err, storage.ErrBlobAuthRequired),
		errors.Is(err, storage.ErrBlobQuarantined):
		return 404, "not_found", "blob not found"
	case errors.Is(err, storage.ErrBlobSoftDeleted),
		errors.Is(err, storage.ErrBlobTombstoned),
		errors.Is(err, storage.ErrKeyShredded):
		return 410, "gone", "content no longer available"
	default:
		return 500, "internal", "internal server error"
	}
}

// Serve dispatches GET /blob/{cid} and GET /blob/{cid}.json by suffix, so the
// behavior does not depend on chi's handling of dotted path segments.
func (h *BlobHandler) Serve(w http.ResponseWriter, r *http.Request) {
	cidParam := chi.URLParam(r, "cid")
	if strings.HasSuffix(cidParam, ".json") {
		h.serveJSON(w, r, strings.TrimSuffix(cidParam, ".json"))
		return
	}
	h.serveBytes(w, r, cidParam)
}

func (h *BlobHandler) setBytesHeaders(w http.ResponseWriter, v *storage.BlobView) {
	w.Header().Set("Content-Type", v.MIME)
	w.Header().Set("ETag", `"`+v.CID+`"`)
	w.Header().Set("X-Nova-Cid", v.CID)
	w.Header().Set("X-Nova-Envelope-Version", strconv.Itoa(int(v.EnvelopeVersion)))
	w.Header().Set("Cache-Control", cacheControl(v.Visibility))
}

func (h *BlobHandler) serveBytes(w http.ResponseWriter, r *http.Request, cidStr string) {
	rid := middleware.RequestIDFromContext(r.Context())
	v, err := h.r.Resolve(r.Context(), cidStr)
	if err != nil {
		status, code, msg := mapBytesError(err)
		httputil.WriteError(w, status, code, msg, rid)
		return
	}

	hasRange := r.Header.Get("Range") != ""
	if hasRange && v.Encrypted {
		httputil.WriteError(w, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable",
			"range requests are not supported for encrypted blobs", rid)
		return
	}

	body, err := h.r.OpenBytes(r.Context(), v)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
		return
	}
	defer body.Close()

	h.setBytesHeaders(w, v)

	if hasRange && !v.Encrypted {
		// M3 buffers the whole plaintext object to satisfy a Range request
		// (the ipfs.Backend.Get reader is not seekable through the interface).
		// Acceptable for Phase 1; a seekable backend reader would let us
		// stream ranges. TODO(post-M3): expose a ReadSeeker for plaintext.
		buf, err := io.ReadAll(body)
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
			return
		}
		http.ServeContent(w, r, "", v.UploadedAt, bytes.NewReader(buf))
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(v.PlaintextSize, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// Head serves HEAD /blob/{cid}: headers only, no body, no decrypt/fetch.
func (h *BlobHandler) Head(w http.ResponseWriter, r *http.Request) {
	v, err := h.r.Resolve(r.Context(), chi.URLParam(r, "cid"))
	if err != nil {
		status, _, _ := mapBytesError(err)
		w.WriteHeader(status)
		return
	}
	h.setBytesHeaders(w, v)
	w.Header().Set("Content-Length", strconv.FormatInt(v.PlaintextSize, 10))
	w.WriteHeader(http.StatusOK)
}

func (h *BlobHandler) serveJSON(w http.ResponseWriter, r *http.Request, cidStr string) {
	rid := middleware.RequestIDFromContext(r.Context())
	v, err := h.r.Resolve(r.Context(), cidStr)
	if err != nil {
		status, code, msg := mapJSONError(err)
		httputil.WriteError(w, status, code, msg, rid)
		return
	}
	out := map[string]any{
		"cid":         v.CID,
		"mime_type":   v.MIME,
		"byte_size":   v.PlaintextSize,
		"uploaded_at": v.UploadedAt.UTC().Format(time.RFC3339),
		"state":       "active",
		"product":     v.Product,
		"urls": map[string]string{
			"bytes": "/blob/" + v.CID,
			"json":  "/blob/" + v.CID + ".json",
		},
	}
	if v.OwnerID != nil {
		out["owner_id"] = *v.OwnerID
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", cacheControl(v.Visibility))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}
