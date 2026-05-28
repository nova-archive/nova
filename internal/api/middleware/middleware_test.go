package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/stretchr/testify/require"
)

func TestRequestIDGeneratesWhenAbsent(t *testing.T) {
	t.Parallel()
	var seen string
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = middleware.RequestIDFromContext(r.Context())
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	require.NotEmpty(t, seen)
	require.Equal(t, seen, rec.Header().Get("X-Request-ID"))
}

func TestRequestIDPropagatesInbound(t *testing.T) {
	t.Parallel()
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "abc-123", middleware.RequestIDFromContext(r.Context()))
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	h.ServeHTTP(rec, req)
	require.Equal(t, "abc-123", rec.Header().Get("X-Request-ID"))
}

func TestRecoverReturns500(t *testing.T) {
	t.Parallel()
	h := middleware.RequestID(middleware.Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	require.Equal(t, 500, rec.Code)
	require.Contains(t, rec.Body.String(), "internal")
	require.NotContains(t, rec.Body.String(), "boom")
}
