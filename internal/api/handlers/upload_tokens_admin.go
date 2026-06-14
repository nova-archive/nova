package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auth/uploadtoken"
	"github.com/nova-archive/nova/internal/db/gen"
)

// UploadTokensAdminHandler serves the operator-only upload-token admin endpoints:
//
//	POST   /api/v1/admin/upload-tokens
//	GET    /api/v1/admin/upload-tokens
//	DELETE /api/v1/admin/upload-tokens/{id}
type UploadTokensAdminHandler struct{ q *gen.Queries }

// NewUploadTokensAdminHandler builds the handler over the generated queries.
func NewUploadTokensAdminHandler(q *gen.Queries) *UploadTokensAdminHandler {
	return &UploadTokensAdminHandler{q: q}
}

// validUploadTokenProducts mirrors the blob_product DB enum values.
var validUploadTokenProducts = map[string]bool{
	"image": true, "video": true, "audio": true, "archive": true, "document": true, "raw": true,
}

type mintUploadTokenRequest struct {
	Label        string `json:"label"`
	CollectionID string `json:"collection_id"`
	Product      string `json:"product"`
	MaxFileSize  *int64 `json:"max_file_size"`
	ExpiresAt    string `json:"expires_at"`
}

type mintUploadTokenResponse struct {
	ID           string  `json:"id"`
	Token        string  `json:"token"`
	Label        *string `json:"label,omitempty"`
	Role         string  `json:"role"`
	CollectionID *string `json:"collection_id,omitempty"`
	Product      *string `json:"product,omitempty"`
	MaxFileSize  *int64  `json:"max_file_size,omitempty"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

// Mint creates a new upload token. The wire token (nova_ut_…) is returned once
// in the 201 response body and never again. Role is always "uploader" — the
// request body may not override it.
func (h *UploadTokensAdminHandler) Mint(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	ctx := r.Context()

	owner := ownerFromContext(ctx)
	if owner == nil {
		httputil.WriteError(w, http.StatusUnauthorized, "unauthenticated", "operator identity required", rid)
		return
	}

	var req mintUploadTokenRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body", rid)
			return
		}
	}

	params := gen.CreateUploadTokenParams{
		// Role is ALWAYS uploader; never accept from the request body.
		Role:      gen.UserRoleUploader,
		CreatedBy: pgtype.UUID{Bytes: *owner, Valid: true},
	}

	if req.Label != "" {
		params.Label = pgtype.Text{String: req.Label, Valid: true}
	}

	if req.CollectionID != "" {
		u, err := uuid.Parse(req.CollectionID)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "collection_id must be a UUID", rid)
			return
		}
		params.CollectionID = pgtype.UUID{Bytes: u, Valid: true}
	}

	if req.Product != "" {
		if !validUploadTokenProducts[req.Product] {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "unknown product value", rid)
			return
		}
		params.Product = gen.NullBlobProduct{BlobProduct: gen.BlobProduct(req.Product), Valid: true}
	}

	if req.MaxFileSize != nil {
		if *req.MaxFileSize <= 0 {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "max_file_size must be positive", rid)
			return
		}
		params.MaxFileSize = pgtype.Int8{Int64: *req.MaxFileSize, Valid: true}
	}

	if req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "expires_at must be RFC3339", rid)
			return
		}
		params.ExpiresAt = pgtype.Timestamptz{Time: t, Valid: true}
	}

	// Generate the secret bytes before inserting so we can build the wire token
	// using the DB-assigned id. The DB assigns its own UUID (gen_random_uuid());
	// we store hash(secret) and then encode wire = nova_ut_<db_id>.<secret>.
	secret, err := uploadtoken.GenerateSecret()
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to generate token", rid)
		return
	}
	params.TokenHash = uploadtoken.HashSecret(secret)

	row, err := h.q.CreateUploadToken(ctx, params)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to create token", rid)
		return
	}

	// Build wire from the DB-assigned id + the raw secret.
	dbID := uuid.UUID(row.ID.Bytes)
	wire := uploadtoken.BuildWire(dbID, secret)

	resp := mintUploadTokenResponse{
		ID:        dbID.String(),
		Token:     wire,
		Role:      string(row.Role),
		CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
	}
	if row.Label.Valid {
		s := row.Label.String
		resp.Label = &s
	}
	if row.CollectionID.Valid {
		s := uuid.UUID(row.CollectionID.Bytes).String()
		resp.CollectionID = &s
	}
	if row.Product.Valid {
		s := string(row.Product.BlobProduct)
		resp.Product = &s
	}
	if row.MaxFileSize.Valid {
		n := row.MaxFileSize.Int64
		resp.MaxFileSize = &n
	}
	if row.ExpiresAt.Valid {
		s := row.ExpiresAt.Time.UTC().Format(time.RFC3339)
		resp.ExpiresAt = &s
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

type uploadTokenListItem struct {
	ID           string  `json:"id"`
	Label        *string `json:"label,omitempty"`
	Role         string  `json:"role"`
	CollectionID *string `json:"collection_id,omitempty"`
	Product      *string `json:"product,omitempty"`
	MaxFileSize  *int64  `json:"max_file_size,omitempty"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
	CreatedBy    string  `json:"created_by"`
	CreatedAt    string  `json:"created_at"`
	LastUsedAt   *string `json:"last_used_at,omitempty"`
	RevokedAt    *string `json:"revoked_at,omitempty"`
}

// List returns all upload tokens, newest-first. Secrets and token_hash are
// never included in the response.
func (h *UploadTokensAdminHandler) List(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	ctx := r.Context()

	rows, err := h.q.ListUploadTokens(ctx)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to list tokens", rid)
		return
	}

	data := make([]uploadTokenListItem, 0, len(rows))
	for _, row := range rows {
		item := uploadTokenListItem{
			ID:        uuid.UUID(row.ID.Bytes).String(),
			Role:      string(row.Role),
			CreatedBy: uuid.UUID(row.CreatedBy.Bytes).String(),
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		}
		if row.Label.Valid {
			s := row.Label.String
			item.Label = &s
		}
		if row.CollectionID.Valid {
			s := uuid.UUID(row.CollectionID.Bytes).String()
			item.CollectionID = &s
		}
		if row.Product.Valid {
			s := string(row.Product.BlobProduct)
			item.Product = &s
		}
		if row.MaxFileSize.Valid {
			n := row.MaxFileSize.Int64
			item.MaxFileSize = &n
		}
		if row.ExpiresAt.Valid {
			s := row.ExpiresAt.Time.UTC().Format(time.RFC3339)
			item.ExpiresAt = &s
		}
		if row.LastUsedAt.Valid {
			s := row.LastUsedAt.Time.UTC().Format(time.RFC3339)
			item.LastUsedAt = &s
		}
		if row.RevokedAt.Valid {
			s := row.RevokedAt.Time.UTC().Format(time.RFC3339)
			item.RevokedAt = &s
		}
		data = append(data, item)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// Revoke sets revoked_at on the token identified by {id}. Returns 204 on
// success, 404 if the token is not found or is already revoked.
func (h *UploadTokensAdminHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	ctx := r.Context()

	rawID := chi.URLParam(r, "id")
	uid, err := uuid.Parse(rawID)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "id must be a UUID", rid)
		return
	}

	affected, err := h.q.RevokeUploadToken(ctx, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to revoke token", rid)
		return
	}
	if affected == 0 {
		httputil.WriteError(w, http.StatusNotFound, "not_found", "token not found or already revoked", rid)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
