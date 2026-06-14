package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/uploadtoken"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
)

// operatorReq builds a request with an operator identity in the context.
func operatorReq(method, path, body string, opID uuid.UUID) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r = r.WithContext(auth.ContextWithIdentity(r.Context(), auth.Identity{
		UserID: opID.String(),
		Role:   "operator",
	}))
	return r
}

func TestIntegrationUploadTokensMint(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	op := seedUserRole(t, ctx, pool, "operator")
	h := handlers.NewUploadTokensAdminHandler(gen.New(pool))

	// Mint with a label.
	rec := httptest.NewRecorder()
	h.Mint(rec, operatorReq(http.MethodPost, "/api/v1/admin/upload-tokens",
		`{"label":"ci-bot"}`, op))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp["id"], "id present")
	token, ok := resp["token"].(string)
	require.True(t, ok, "token field is a string")
	require.True(t, strings.HasPrefix(token, "nova_ut_"), "wire token has correct prefix")
	require.Equal(t, "uploader", resp["role"], "role is always uploader")
	require.Equal(t, "ci-bot", resp["label"], "label echoed")

	// Secret is never returned again: a second mint returns a different token.
	rec2 := httptest.NewRecorder()
	h.Mint(rec2, operatorReq(http.MethodPost, "/api/v1/admin/upload-tokens",
		`{}`, op))
	require.Equal(t, http.StatusCreated, rec2.Code)
	var resp2 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	require.NotEqual(t, token, resp2["token"], "each mint returns a distinct token")

	// Reject invalid expires_at.
	rec3 := httptest.NewRecorder()
	h.Mint(rec3, operatorReq(http.MethodPost, "/api/v1/admin/upload-tokens",
		`{"expires_at":"not-a-date"}`, op))
	require.Equal(t, http.StatusBadRequest, rec3.Code)

	// Reject invalid collection_id.
	rec4 := httptest.NewRecorder()
	h.Mint(rec4, operatorReq(http.MethodPost, "/api/v1/admin/upload-tokens",
		`{"collection_id":"not-a-uuid"}`, op))
	require.Equal(t, http.StatusBadRequest, rec4.Code)

	// Reject invalid max_file_size (<= 0).
	rec5 := httptest.NewRecorder()
	h.Mint(rec5, operatorReq(http.MethodPost, "/api/v1/admin/upload-tokens",
		`{"max_file_size":-1}`, op))
	require.Equal(t, http.StatusBadRequest, rec5.Code)
}

func TestIntegrationUploadTokensList(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	op := seedUserRole(t, ctx, pool, "operator")
	h := handlers.NewUploadTokensAdminHandler(gen.New(pool))

	// Mint two tokens.
	h.Mint(httptest.NewRecorder(), operatorReq(http.MethodPost, "/api/v1/admin/upload-tokens",
		`{"label":"first"}`, op))
	h.Mint(httptest.NewRecorder(), operatorReq(http.MethodPost, "/api/v1/admin/upload-tokens",
		`{"label":"second"}`, op))

	// List returns both without any secret / token_hash.
	rec := httptest.NewRecorder()
	h.List(rec, operatorReq(http.MethodGet, "/api/v1/admin/upload-tokens", "", op))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var listResp struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	require.Len(t, listResp.Data, 2)

	for _, item := range listResp.Data {
		_, hasHash := item["token_hash"]
		require.False(t, hasHash, "token_hash must not appear in list response")
		_, hasToken := item["token"]
		require.False(t, hasToken, "raw token must not appear in list response")
		require.NotEmpty(t, item["id"], "id present")
		require.Equal(t, "uploader", item["role"])
	}
}

func TestIntegrationUploadTokensRevoke(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	op := seedUserRole(t, ctx, pool, "operator")
	h := handlers.NewUploadTokensAdminHandler(gen.New(pool))

	// Mint a token.
	rec := httptest.NewRecorder()
	h.Mint(rec, operatorReq(http.MethodPost, "/api/v1/admin/upload-tokens",
		`{"label":"revoke-me"}`, op))
	require.Equal(t, http.StatusCreated, rec.Code)

	var mintResp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &mintResp))
	tokenID := mintResp["id"].(string)
	wireToken := mintResp["token"].(string)

	// Revoke.
	rec2 := httptest.NewRecorder()
	req := operatorReq(http.MethodDelete, "/api/v1/admin/upload-tokens/"+tokenID, "", op)
	req = withURLParam(req, "id", tokenID)
	h.Revoke(rec2, req)
	require.Equal(t, http.StatusNoContent, rec2.Code, rec2.Body.String())

	// After revoke: verify the upload token fails.
	verifier := uploadtoken.New(gen.New(pool))
	_, err := verifier.Verify(ctx, wireToken)
	require.Error(t, err, "revoked token must not verify")
	require.Contains(t, err.Error(), "revoked")

	// Revoking again returns 404 (already revoked, 0 rows affected).
	rec3 := httptest.NewRecorder()
	req3 := operatorReq(http.MethodDelete, "/api/v1/admin/upload-tokens/"+tokenID, "", op)
	req3 = withURLParam(req3, "id", tokenID)
	h.Revoke(rec3, req3)
	require.Equal(t, http.StatusNotFound, rec3.Code)

	// Revoke with bad UUID returns 400.
	rec4 := httptest.NewRecorder()
	req4 := operatorReq(http.MethodDelete, "/api/v1/admin/upload-tokens/not-a-uuid", "", op)
	req4 = withURLParam(req4, "id", "not-a-uuid")
	h.Revoke(rec4, req4)
	require.Equal(t, http.StatusBadRequest, rec4.Code)
}
