package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/config/reload"
)

// TestConfigPatchHotAppliesCORSThroughStore proves that an operator PATCH to the
// config admin API flows through the live store into the reloadable CORS
// middleware with no restart: a request that was rejected (no CORS) before the
// patch is served the new allowed origin immediately after.
func TestConfigPatchHotAppliesCORSThroughStore(t *testing.T) {
	// Start with CORS disabled.
	p := writeTempCfg(t) // reuse the helper from config_admin_test.go
	cfg, err := config.LoadFromFile(p)
	require.NoError(t, err)
	store := reload.New(cfg, nil, nil)

	corsMW := middleware.CORSReloadable(store)
	upstream := corsMW(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	optic := func(origin string) string {
		r := httptest.NewRequest(http.MethodOptions, "/api/v1/uploads", nil)
		r.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		upstream.ServeHTTP(rec, r)
		return rec.Header().Get("Access-Control-Allow-Origin")
	}
	require.Empty(t, optic("https://w.test")) // CORS disabled initially

	h := handlers.NewConfigAdminHandler(store, p)
	rec := httptest.NewRecorder()
	h.Update(rec, operatorReq(http.MethodPatch, "/api/v1/admin/config",
		`{"uploads":{"cors":{"enabled":true,"allowed_origins":["https://w.test"]}}}`, uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The CORS middleware now serves the new origin live — no restart.
	require.Equal(t, "https://w.test", optic("https://w.test"))
}
