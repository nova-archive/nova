package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/ratelimit"
)

// RateLimit returns middleware that rejects requests exceeding the per-IP
// limiter with 429. The client IP is taken from X-Forwarded-For (nginx) and
// falls back to RemoteAddr.
func RateLimit(l *ratelimit.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(clientIP(r)) {
				api.WriteError(w, http.StatusTooManyRequests, "rate_limited",
					"too many requests", RequestIDFromContext(r.Context()))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
