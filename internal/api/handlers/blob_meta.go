package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/lifecycle"
)

// tsLayout matches the audits_admin timestamp rendering (RFC3339 with optional
// sub-second precision).
const tsLayout = "2006-01-02T15:04:05.999999Z07:00"

// BlobMetaHandler serves the authenticated owner/operator blob routes (M11),
// which M6 deferred to this milestone:
//
//	GET    /api/v1/blobs/{cid}   metadata read in any state (owner OR operator/moderator)
//	DELETE /api/v1/blobs/{cid}   owner soft-delete (owner OR operator)
type BlobMetaHandler struct {
	q    *gen.Queries
	life *lifecycle.Service
}

// NewBlobMetaHandler builds the handler over the generated queries + the
// content-lifecycle service.
func NewBlobMetaHandler(q *gen.Queries, life *lifecycle.Service) *BlobMetaHandler {
	return &BlobMetaHandler{q: q, life: life}
}

type blobMetaItem struct {
	CID              string  `json:"cid"`
	OwnerID          *string `json:"owner_id"`
	ParentCID        *string `json:"parent_cid"`
	DerivativePreset *string `json:"derivative_preset"`
	DerivativeFormat *string `json:"derivative_format"`
	MIMEType         string  `json:"mime_type"`
	ByteSize         int64   `json:"byte_size"`
	State            string  `json:"state"`
	Product          string  `json:"product"`
	UploadedAt       string  `json:"uploaded_at"`
	SoftDeletedAt    *string `json:"soft_deleted_at"`
}

// Get returns blob metadata in ANY state (unlike the public read path, which
// rejects non-active states). Authorization: the blob's owner, or an
// operator/moderator.
func (h *BlobMetaHandler) Get(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	id, ok := auth.IdentityFromContext(r.Context())
	if !ok { // defensive: route is guarded by RequireAuthenticated
		httputil.WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required", rid)
		return
	}
	meta, err := h.q.GetBlobMeta(r.Context(), chi.URLParam(r, "cid"))
	if errors.Is(err, pgx.ErrNoRows) {
		httputil.WriteError(w, http.StatusNotFound, "not_found", "blob not found", rid)
		return
	}
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to read blob", rid)
		return
	}
	if !ownerOrRole(id, meta.OwnerID, "operator", "moderator") {
		httputil.WriteError(w, http.StatusForbidden, "forbidden", "not permitted", rid)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(blobMetaToItem(meta))
}

// Delete soft-deletes a blob (owner OR operator; moderators may read but not
// delete). A non-active blob returns 409 not_active.
func (h *BlobMetaHandler) Delete(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	id, ok := auth.IdentityFromContext(r.Context())
	if !ok {
		httputil.WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required", rid)
		return
	}
	cidStr := chi.URLParam(r, "cid")
	meta, err := h.q.GetBlobMeta(r.Context(), cidStr)
	if errors.Is(err, pgx.ErrNoRows) {
		httputil.WriteError(w, http.StatusNotFound, "not_found", "blob not found", rid)
		return
	}
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to read blob", rid)
		return
	}
	if !ownerOrRole(id, meta.OwnerID, "operator") {
		httputil.WriteError(w, http.StatusForbidden, "forbidden", "not permitted", rid)
		return
	}

	var actor *uuid.UUID
	if uid, perr := uuid.Parse(id.UserID); perr == nil {
		actor = &uid
	}
	switch err := h.life.SoftDelete(r.Context(), cidStr, actor); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, lifecycle.ErrNotActive):
		httputil.WriteError(w, http.StatusConflict, "not_active", "blob is not active", rid)
	default:
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to soft-delete", rid)
	}
}

// ownerOrRole reports whether the identity owns the blob (owner_id matches the
// token subject) or holds one of the elevated roles. ownerID is the coalesced
// projection (” = no owner).
func ownerOrRole(id auth.Identity, ownerID string, roles ...string) bool {
	if ownerID != "" && id.UserID == ownerID {
		return true
	}
	for _, role := range roles {
		if id.Role == role {
			return true
		}
	}
	return false
}

func blobMetaToItem(m gen.GetBlobMetaRow) blobMetaItem {
	item := blobMetaItem{
		CID:        m.Cid,
		MIMEType:   m.MimeType,
		ByteSize:   m.ByteSize,
		State:      m.State,
		Product:    m.Product,
		UploadedAt: m.UploadedAt.UTC().Format(tsLayout),
	}
	if m.OwnerID != "" {
		o := m.OwnerID
		item.OwnerID = &o
	}
	if m.ParentCid.Valid {
		v := m.ParentCid.String
		item.ParentCID = &v
	}
	if m.DerivativePreset.Valid {
		v := m.DerivativePreset.String
		item.DerivativePreset = &v
	}
	if m.DerivativeFormat.Valid {
		v := m.DerivativeFormat.String
		item.DerivativeFormat = &v
	}
	if m.SoftDeletedAt.Valid {
		v := m.SoftDeletedAt.Time.UTC().Format(tsLayout)
		item.SoftDeletedAt = &v
	}
	return item
}
