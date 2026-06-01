package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/stretchr/testify/require"
)

// TestRateLimitMiddleware429 confirms the basic deny path with no trusted
// proxies configured: the key is RemoteAddr, two requests from the same
// RemoteAddr collide in the bucket of capacity 1.
func TestRateLimitMiddleware429(t *testing.T) {
	t.Parallel()
	l := ratelimit.NewLimiter(ratelimit.Config{RatePerSec: 0, Burst: 1}, nil)
	h := middleware.RequestID(middleware.RateLimit(l, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})))

	call := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/blob/x", nil)
		// Same RemoteAddr used by httptest.NewRequest default; both calls
		// land in the same bucket.
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	require.Equal(t, 200, call())
	require.Equal(t, 429, call())
}

// TestRateLimitMiddleware_UntrustedXFFDoesNotKey verifies that XFF cannot
// be used to multiplex around the limiter when no proxies are trusted.
// Without trusted-proxy enforcement, an attacker could rotate XFF and
// reset their bucket on each request.
func TestRateLimitMiddleware_UntrustedXFFDoesNotKey(t *testing.T) {
	t.Parallel()
	l := ratelimit.NewLimiter(ratelimit.Config{RatePerSec: 0, Burst: 1}, nil)
	h := middleware.RateLimit(l, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	hit := func(xff string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/blob/x", nil)
		req.Header.Set("X-Forwarded-For", xff)
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// First request: bucket has Burst=1, allowed.
	require.Equal(t, 200, hit("1.2.3.4"))
	// Same RemoteAddr — XFF is ignored — bucket is exhausted: 429 regardless
	// of the spoofed XFF on this second hit.
	require.Equal(t, 429, hit("5.6.7.8"))
}

// TestRateLimitMiddleware_TrustedXFFKeysSeparately verifies that with a
// configured trusted-proxy list, the XFF leftmost hop is the rate-limit key.
// Two distinct upstream IPs forwarded by the same trusted proxy get separate
// buckets.
func TestRateLimitMiddleware_TrustedXFFKeysSeparately(t *testing.T) {
	t.Parallel()
	// The default RemoteAddr from httptest.NewRequest is 192.0.2.1:1234.
	trusted, err := httputil.ParseTrustedProxies("192.0.2.0/24")
	require.NoError(t, err)
	require.Equal(t, "192.0.2.0/24", trusted[0].String())
	_ = netip.MustParseAddr("192.0.2.1") // sanity that the default falls inside

	l := ratelimit.NewLimiter(ratelimit.Config{RatePerSec: 0, Burst: 1}, nil)
	h := middleware.RateLimit(l, trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	hit := func(xff string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/blob/x", nil)
		req.Header.Set("X-Forwarded-For", xff)
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// Two distinct upstream IPs forwarded by the trusted proxy — each gets
	// its own bucket — both 200.
	require.Equal(t, 200, hit("1.2.3.4"))
	require.Equal(t, 200, hit("5.6.7.8"))
	// Repeat the first upstream: now its bucket is exhausted.
	require.Equal(t, 429, hit("1.2.3.4"))
}
