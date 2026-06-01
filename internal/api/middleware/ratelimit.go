package middleware

import (
	"net/http"
	"net/netip"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/ratelimit"
)

// RateLimit returns middleware that rejects requests exceeding the per-IP
// limiter with 429. The client IP is resolved via httputil.ClientIPString
// which honors X-Forwarded-For only when r.RemoteAddr matches a configured
// trusted proxy (see NOVA_TRUSTED_PROXIES in cmd/coordinator). Direct-
// exposure deployments (no trusted proxies configured) fall back to
// RemoteAddr, which prevents XFF spoofing from bypassing the limiter.
func RateLimit(l *ratelimit.Limiter, trustedProxies []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := httputil.ClientIPString(r, trustedProxies)
			if key == "" {
				// Unparseable RemoteAddr: cannot rate-limit. Fail open to
				// avoid wedging the entire ingress on a malformed client.
				next.ServeHTTP(w, r)
				return
			}
			if !l.Allow(key) {
				httputil.WriteError(w, http.StatusTooManyRequests, "rate_limited",
					"too many requests", RequestIDFromContext(r.Context()))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
