package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/db/gen"
)

// BlobsAdminHandler serves GET /api/v1/admin/blobs (M11): an operator-wide,
// paginated, filterable listing of blobs in any state. The owner-facing detail
// view is GET /api/v1/blobs/{cid} (BlobMetaHandler).
type BlobsAdminHandler struct{ q *gen.Queries }

// NewBlobsAdminHandler builds the handler over the generated queries.
func NewBlobsAdminHandler(q *gen.Queries) *BlobsAdminHandler { return &BlobsAdminHandler{q: q} }

var validBlobStates = map[string]bool{
	"active": true, "soft_deleted": true, "quarantined": true, "tombstoned": true,
}

var validBlobProducts = map[string]bool{
	"image": true, "video": true, "audio": true, "archive": true, "document": true, "raw": true,
}

type blobListItem struct {
	CID        string  `json:"cid"`
	OwnerID    *string `json:"owner_id"`
	ParentCID  *string `json:"parent_cid"`
	MIMEType   string  `json:"mime_type"`
	ByteSize   int64   `json:"byte_size"`
	State      string  `json:"state"`
	Product    string  `json:"product"`
	UploadedAt string  `json:"uploaded_at"`
}

// List returns a page of blobs, newest-first, optionally filtered by state,
// product, and owner_id.
func (h *BlobsAdminHandler) List(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())

	pg, err := httputil.ParsePage(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), rid)
		return
	}

	listParams := gen.ListBlobsParams{Lim: int32(pg.Limit), Off: int32(pg.Offset)}
	countParams := gen.CountBlobsParams{}

	if v := r.URL.Query().Get("state"); v != "" {
		if !validBlobStates[v] {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "unknown state filter", rid)
			return
		}
		listParams.State = pgtype.Text{String: v, Valid: true}
		countParams.State = pgtype.Text{String: v, Valid: true}
	}
	if v := r.URL.Query().Get("product"); v != "" {
		if !validBlobProducts[v] {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "unknown product filter", rid)
			return
		}
		listParams.Product = pgtype.Text{String: v, Valid: true}
		countParams.Product = pgtype.Text{String: v, Valid: true}
	}
	if v := r.URL.Query().Get("owner_id"); v != "" {
		uid, perr := uuid.Parse(v)
		if perr != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "owner_id must be a uuid", rid)
			return
		}
		listParams.Owner = pgtype.UUID{Bytes: uid, Valid: true}
		countParams.Owner = pgtype.UUID{Bytes: uid, Valid: true}
	}

	rows, err := h.q.ListBlobs(r.Context(), listParams)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to list blobs", rid)
		return
	}
	total, err := h.q.CountBlobs(r.Context(), countParams)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to count blobs", rid)
		return
	}

	data := make([]blobListItem, 0, len(rows))
	for _, row := range rows {
		item := blobListItem{
			CID:        row.Cid,
			MIMEType:   row.MimeType,
			ByteSize:   row.ByteSize,
			State:      row.State,
			Product:    row.Product,
			UploadedAt: row.UploadedAt.UTC().Format(tsLayout),
		}
		if row.OwnerID != "" {
			o := row.OwnerID
			item.OwnerID = &o
		}
		if row.ParentCid.Valid {
			v := row.ParentCid.String
			item.ParentCID = &v
		}
		data = append(data, item)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"pagination": httputil.Pagination{
			Page:    pg.Page,
			PerPage: pg.PerPage,
			Total:   int(total),
		},
	})
}
