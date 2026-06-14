package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/config/reload"
	"github.com/stretchr/testify/require"
)

func TestCORSEchoesAllowlistedOrigin(t *testing.T) {
	t.Parallel()
	mw := middleware.CORS(config.CORS{Enabled: true, AllowedOrigins: []string{"https://x.test"}})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest("POST", "/api/v1/uploads", nil)
	req.Header.Set("Origin", "https://x.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, "https://x.test", rec.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, rec.Header().Values("Vary"), "Origin")
}

func TestCORSPreflight204(t *testing.T) {
	t.Parallel()
	mw := middleware.CORS(config.CORS{
		Enabled:        true,
		AllowedOrigins: []string{"https://x.test"},
		AllowedMethods: []string{"POST", "PATCH", "DELETE"},
		AllowedHeaders: []string{"Content-Type", "Upload-Offset"},
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/uploads", nil)
	req.Header.Set("Origin", "https://x.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Access-Control-Allow-Methods"))
	require.NotEmpty(t, rec.Header().Get("Access-Control-Allow-Headers"))
	require.Equal(t, "600", rec.Header().Get("Access-Control-Max-Age"))
}

func TestCORSDisabledNoHeaders(t *testing.T) {
	t.Parallel()
	mw := middleware.CORS(config.CORS{Enabled: false, AllowedOrigins: []string{"https://x.test"}})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest("POST", "/api/v1/uploads", nil)
	req.Header.Set("Origin", "https://x.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestCORSForeignOriginRejected(t *testing.T) {
	t.Parallel()
	mw := middleware.CORS(config.CORS{Enabled: true, AllowedOrigins: []string{"https://x.test"}})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest("POST", "/api/v1/uploads", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(t, http.StatusOK, rec.Code)
}

func corsCfg(t *testing.T, origins []string) *config.Config {
	t.Helper()
	doc := "operator:\n  hostname: h.test\n  contact_email: a@b.test\n" +
		"tls:\n  mode: dev-self-signed\n" +
		"orchestrator:\n  replication:\n    factor:\n      important: 2\n" +
		"uploads:\n  cors:\n    enabled: true\n    allowed_origins:\n"
	for _, o := range origins {
		doc += "      - " + o + "\n"
	}
	cfg, err := config.LoadFromBytes([]byte(doc))
	require.NoError(t, err)
	return cfg
}

func TestCORSReloadableFlipsOnVersionChange(t *testing.T) {
	store := reload.New(corsCfg(t, []string{"https://a.test"}), nil, nil)
	h := middleware.CORSReloadable(store)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))

	do := func(origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/uploads", nil)
		r.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	require.Equal(t, "https://a.test", do("https://a.test").Header().Get("Access-Control-Allow-Origin"))
	require.Empty(t, do("https://b.test").Header().Get("Access-Control-Allow-Origin"))

	// Swap in a config that allows b.test; the next request reflects it live.
	store.Swap(corsCfg(t, []string{"https://b.test"}))
	require.Equal(t, "https://b.test", do("https://b.test").Header().Get("Access-Control-Allow-Origin"))
}
