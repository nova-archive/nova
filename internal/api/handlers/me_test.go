package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestMeReturnsUser(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	u, err := q.CreateUser(ctx, gen.CreateUserParams{
		Email:        "me@example.com",
		Role:         gen.UserRole("operator"),
		PasswordHash: pgtype.Text{},
	})
	require.NoError(t, err)
	uid := uuid.UUID(u.ID.Bytes)

	h := handlers.NewMeHandler(q)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	r = r.WithContext(auth.ContextWithIdentity(ctx, auth.Identity{UserID: uid.String(), Role: "operator"}))
	rr := httptest.NewRecorder()
	h.Get(rr, r)

	require.Equal(t, http.StatusOK, rr.Code)
	var out struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Equal(t, uid.String(), out.ID)
	require.Equal(t, "operator", out.Role)
}
