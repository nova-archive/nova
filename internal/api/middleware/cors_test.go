package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/config"
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
