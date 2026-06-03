package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/upload"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

const tusVersion = "1.0.0"

// SessionStore is the tus surface the handler depends on (satisfied by
// *upload.Store).
type SessionStore interface {
	Create(ctx context.Context, p upload.CreateParams) (uuid.UUID, error)
	Get(ctx context.Context, id uuid.UUID) (*upload.Session, error)
	AppendChunk(ctx context.Context, id uuid.UUID, clientOffset int64, r io.Reader) (int64, error)
	Finalize(ctx context.Context, id uuid.UUID) (*storage.PutResult, error)
	Abort(ctx context.Context, id uuid.UUID) error
}

// UploadHandler serves the tus endpoints (/api/v1/uploads*) and the multipart
// fallback (/api/v1/blobs).
type UploadHandler struct {
	store           SessionStore
	put             upload.Committer
	maxUploadSize   int64
	recordIP        bool
	trustedProxies  []netip.Prefix
	imageAccepts    func(mime string) bool
	imagePresetURLs func(cid string) map[string]string
}

// NewUploadHandler builds the upload handler. recordIP=false (paranoid mode)
// suppresses source-IP capture on the multipart path. trustedProxies gates
// X-Forwarded-For trust when resolving the client IP for source_ip recording
// (see httputil.ClientIP); nil/empty means XFF is always ignored.
func NewUploadHandler(store SessionStore, put upload.Committer, maxUploadSize int64, recordIP bool, trustedProxies []netip.Prefix) *UploadHandler {
	return &UploadHandler{
		store:          store,
		put:            put,
		maxUploadSize:  maxUploadSize,
		recordIP:       recordIP,
		trustedProxies: trustedProxies,
	}
}

// SetImageHooks injects the nova-image upload-edge hooks: an accept-predicate
// for the early 415 pre-filter, and a preset-URL builder for the result body.
// The coordinator calls this when the image product registers; until then the
// /api/v1/images route behaves like a plain multipart upload forced to product
// "image".
func (h *UploadHandler) SetImageHooks(accepts func(mime string) bool, presetURLs func(cid string) map[string]string) {
	h.imageAccepts = accepts
	h.imagePresetURLs = presetURLs
}

// CreateTus handles POST /api/v1/uploads (tus Creation extension).
func (h *UploadHandler) CreateTus(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	if r.Header.Get("Tus-Resumable") != tusVersion {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "Tus-Resumable: 1.0.0 required", rid)
		return
	}
	length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil || length < 0 {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "valid Upload-Length required", rid)
		return
	}
	if length > h.maxUploadSize {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds max size", rid)
		return
	}
	meta := parseUploadMetadata(r.Header.Get("Upload-Metadata"))
	p := upload.CreateParams{DeclaredLength: length, MIME: meta["mime_type"], Product: meta["product"]}
	if cidStr := meta["collection_id"]; cidStr != "" {
		col, err := uuid.Parse(cidStr)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "bad_request", "collection_id must be a uuid", rid)
			return
		}
		p.CollectionID = &col
	}
	p.OwnerID = ownerFromContext(r.Context())
	id, err := h.store.Create(r.Context(), p)
	if err != nil {
		if errors.Is(err, upload.ErrTooLarge) {
			httputil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds max size", rid)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
		return
	}
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Location", "/api/v1/uploads/"+id.String())
	w.Header().Set("Upload-Offset", "0")
	w.WriteHeader(http.StatusCreated)
}

// HeadTus handles HEAD /api/v1/uploads/{id} (tus offset probe).
func (h *UploadHandler) HeadTus(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	sess, err := h.store.Get(r.Context(), id)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Upload-Offset", strconv.FormatInt(sess.OffsetBytes, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(sess.DeclaredLength, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// PatchTus handles PATCH /api/v1/uploads/{id} (tus Core chunk append).
func (h *UploadHandler) PatchTus(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if r.Header.Get("Content-Type") != "application/offset+octet-stream" {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "Content-Type must be application/offset+octet-stream", rid)
		return
	}
	clientOffset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || clientOffset < 0 {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "valid Upload-Offset required", rid)
		return
	}
	newOffset, err := h.store.AppendChunk(r.Context(), id, clientOffset, r.Body)
	switch {
	case errors.Is(err, upload.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		return
	case errors.Is(err, upload.ErrConflict):
		httputil.WriteError(w, http.StatusConflict, "offset_conflict", "upload offset conflict", rid)
		return
	case err != nil:
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
		return
	}
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
	w.WriteHeader(http.StatusNoContent)
}

// DeleteTus handles DELETE /api/v1/uploads/{id} (abandon).
func (h *UploadHandler) DeleteTus(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.store.Abort(r.Context(), id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// FinalizeTus handles POST /api/v1/uploads/{id}/finalize.
func (h *UploadHandler) FinalizeTus(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	res, err := h.store.Finalize(r.Context(), id)
	switch {
	case errors.Is(err, upload.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		return
	case errors.Is(err, upload.ErrIncomplete):
		httputil.WriteError(w, http.StatusConflict, "upload_incomplete", "upload not yet complete", rid)
		return
	case err != nil:
		h.writePutError(w, err, rid)
		return
	}
	writeUploadResult(w, http.StatusOK, res, nil)
}

// Multipart handles POST /api/v1/blobs (multipart/form-data fallback). product
// comes from the form (default "raw").
func (h *UploadHandler) Multipart(w http.ResponseWriter, r *http.Request) {
	h.multipart(w, r, "")
}

// MultipartImage handles POST /api/v1/images: identical to Multipart but forces
// product "image", applies the image accept-predicate (early 415), and includes
// preset URLs in the result.
func (h *UploadHandler) MultipartImage(w http.ResponseWriter, r *http.Request) {
	h.multipart(w, r, "image")
}

// multipart is the shared body. forceProduct, when non-empty, overrides the
// form's product field and enables the image edge behavior.
func (h *UploadHandler) multipart(w http.ResponseWriter, r *http.Request, forceProduct string) {
	rid := middleware.RequestIDFromContext(r.Context())

	// M6.2 B7 — bound the request body BEFORE multipart parsing so an attacker
	// who declares a small file part but streams a huge body cannot exhaust
	// disk via the multipart spill-to-file path. The +1 MiB slack covers
	// boundary headers and other form fields; ParseMultipartForm will surface
	// the limit as *http.MaxBytesError, which we map to 413.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadSize+(1<<20))

	if err := r.ParseMultipartForm(8 << 20); err != nil { // 8 MiB in-memory; rest spills to disk
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			httputil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds max size", rid)
			return
		}
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "invalid multipart form", rid)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "file field required", rid)
		return
	}
	defer file.Close()
	if header.Size > h.maxUploadSize {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds max size", rid)
		return
	}

	// Defense-in-depth: bound the file reader handed to Put. Today Put uses
	// io.ReadFull(declaredSize), so this is a no-op for the current call site;
	// if a future caller reads-until-EOF, the LimitReader prevents a small-
	// declared / large-streamed mismatch from blowing past the size cap.
	boundedFile := io.LimitReader(file, h.maxUploadSize)

	product := forceProduct
	if product == "" {
		product = r.FormValue("product")
		if product == "" {
			product = "raw"
		}
	}

	declaredMIME := header.Header.Get("Content-Type")

	// Image edge: cheap pre-filter on the declared type before doing any work.
	// The authoritative decode-based check happens in the product's AnalyzeUpload.
	if forceProduct == "image" && h.imageAccepts != nil && !h.imageAccepts(declaredMIME) {
		httputil.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "declared content-type is not an accepted image format", rid)
		return
	}

	pc := storage.PutContext{MIME: declaredMIME, Product: product}
	if h.recordIP {
		pc.SourceIP = httputil.ClientIP(r, h.trustedProxies)
	}
	if cidStr := r.FormValue("collection_id"); cidStr != "" {
		col, err := uuid.Parse(cidStr)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "bad_request", "collection_id must be a uuid", rid)
			return
		}
		pc.CollectionID = &col
	}
	pc.OwnerID = ownerFromContext(r.Context())

	res, err := h.put.Put(r.Context(), boundedFile, header.Size, pc)
	if err != nil {
		h.writePutError(w, err, rid)
		return
	}

	var presets map[string]string
	if product == "image" && h.imagePresetURLs != nil {
		presets = h.imagePresetURLs(res.CID)
	}
	writeUploadResult(w, http.StatusCreated, res, presets)
}

func (h *UploadHandler) writePutError(w http.ResponseWriter, err error, rid string) {
	switch {
	case errors.Is(err, storage.ErrUploadTooLarge):
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds max size", rid)
	case errors.Is(err, storage.ErrMimeRejected):
		httputil.WriteError(w, http.StatusBadRequest, "mime_rejected", "declared content-type contradicts content", rid)
	case errors.Is(err, storage.ErrCollectionNotFound):
		httputil.WriteError(w, http.StatusNotFound, "not_found", "collection not found", rid)
	case errors.Is(err, storage.ErrServerBusy):
		w.Header().Set("Retry-After", "2")
		httputil.WriteError(w, http.StatusServiceUnavailable, "server_busy", "server at capacity, retry", rid)
	case errors.Is(err, storage.ErrModerationRejected):
		httputil.WriteError(w, http.StatusUnprocessableEntity, "moderation_rejected", "upload rejected by moderation", rid)
	case errors.Is(err, storage.ErrBlobBlocklisted):
		httputil.WriteError(w, http.StatusUnavailableForLegalReasons, "blocklisted", "content blocked by moderation blocklist", rid)
	default:
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
	}
}

// writeUploadResult emits the openapi UploadResult JSON with the Nova headers.
// presets (non-nil for image uploads) is emitted under urls.presets.
func writeUploadResult(w http.ResponseWriter, status int, res *storage.PutResult, presets map[string]string) {
	w.Header().Set("X-Nova-Cid", res.CID)
	w.Header().Set("X-Nova-Envelope-Version", "1")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	urls := map[string]any{
		"original": "/blob/" + res.CID,
		"json":     "/blob/" + res.CID + ".json",
	}
	if len(presets) > 0 {
		urls["presets"] = presets
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"cid":       res.CID,
		"byte_size": res.ByteSize,
		"mime_type": res.MIME,
		"product":   res.Product,
		"urls":      urls,
	})
}

// parseUploadMetadata decodes the tus Upload-Metadata header
// ("key b64val,key2 b64val2"). Unknown/blank keys (e.g. filename) are kept but
// the handler only reads mime_type, product, and collection_id.
func parseUploadMetadata(h string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(h, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, " ", 2)
		key := kv[0]
		if len(kv) == 2 {
			if dec, err := base64.StdEncoding.DecodeString(kv[1]); err == nil {
				out[key] = string(dec)
			}
		} else {
			out[key] = ""
		}
	}
	return out
}

func parseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return uuid.Nil, false
	}
	return id, true
}

// ownerFromContext resolves the authenticated user UUID from the request context.
// Returns nil when no identity is present or UserID is not a valid UUID.
func ownerFromContext(ctx context.Context) *uuid.UUID {
	if id, ok := auth.IdentityFromContext(ctx); ok && id.UserID != "" {
		if u, err := uuid.Parse(id.UserID); err == nil {
			return &u
		}
	}
	return nil
}
