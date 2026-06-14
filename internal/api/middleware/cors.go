package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/nova-archive/nova/internal/config"
)

// CORS returns middleware that enforces config-driven cross-origin resource
// sharing on the upload routes. When cfg.Enabled is false it is a no-op
// passthrough. When enabled, only origins in cfg.AllowedOrigins receive CORS
// headers (exact-match). Preflight OPTIONS requests are answered with 204
// before reaching auth guards, allowing browsers to pre-flight without a token.
func CORS(cfg config.CORS) func(http.Handler) http.Handler {
	if !cfg.Enabled {
		return func(next http.Handler) http.Handler { return next }
	}

	// Pre-build lookup set for O(1) origin matching.
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowed[o] = struct{}{}
	}

	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")
	exposed := strings.Join(cfg.ExposedHeaders, ", ")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			_, matched := allowed[origin]

			if matched {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				if cfg.AllowCredentials {
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}

				if r.Method == http.MethodOptions {
					// Preflight: short-circuit with 204 before auth guards.
					if methods != "" {
						w.Header().Set("Access-Control-Allow-Methods", methods)
					}
					if headers != "" {
						w.Header().Set("Access-Control-Allow-Headers", headers)
					}
					w.Header().Set("Access-Control-Max-Age", strconv.Itoa(600))
					w.WriteHeader(http.StatusNoContent)
					return
				}

				// Non-preflight matched request: expose headers if configured.
				if exposed != "" {
					w.Header().Set("Access-Control-Expose-Headers", exposed)
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}
