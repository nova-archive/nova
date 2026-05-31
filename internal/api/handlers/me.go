package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/db/gen"
)

// MeHandler serves GET /api/v1/users/me (openapi getCurrentUser).
type MeHandler struct{ q *gen.Queries }

// NewMeHandler builds the handler over the generated queries.
func NewMeHandler(q *gen.Queries) *MeHandler { return &MeHandler{q: q} }

// Get returns the authenticated user's profile as the openapi User schema.
func (h *MeHandler) Get(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	id, ok := auth.IdentityFromContext(r.Context())
	if !ok { // defensive: route is guarded by RequireAuthenticated
		httputil.WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required", rid)
		return
	}
	uid, err := uuid.Parse(id.UserID)
	if err != nil {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_token", "subject is not a uuid", rid)
		return
	}
	u, err := h.q.GetUserByID(r.Context(), pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "not_found", "user not found", rid)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":         uuid.UUID(u.ID.Bytes).String(),
		"email":      u.Email,
		"role":       string(u.Role),
		"created_at": u.CreatedAt,
		"updated_at": u.UpdatedAt,
	})
}
