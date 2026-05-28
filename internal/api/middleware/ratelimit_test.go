package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/stretchr/testify/require"
)

func TestRateLimitMiddleware429(t *testing.T) {
	t.Parallel()
	l := ratelimit.NewLimiter(ratelimit.Config{RatePerSec: 0, Burst: 1}, nil)
	h := middleware.RequestID(middleware.RateLimit(l)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})))

	call := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/blob/x", nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	require.Equal(t, 200, call())
	require.Equal(t, 429, call())
}
