package middleware

import (
	"log/slog"
	"net/http"

	"github.com/nova-archive/nova/internal/api/httputil"
)

// Recover converts a panic in a downstream handler into a 500 JSON Error,
// logging the panic locally without leaking it to the client.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				rid := RequestIDFromContext(r.Context())
				slog.Error("panic in handler", "request_id", rid, "panic", rec, "path", r.URL.Path)
				httputil.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
