package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/config/reload"
)

// compiledCORS holds the pre-joined header strings and the origin lookup set
// derived from a config.CORS snapshot.
type compiledCORS struct {
	enabled          bool
	allowed          map[string]struct{}
	methods, headers string
	exposed          string
	allowCredentials bool
}

func compile(c config.CORS) *compiledCORS {
	cc := &compiledCORS{
		enabled:          c.Enabled,
		allowCredentials: c.AllowCredentials,
		methods:          strings.Join(c.AllowedMethods, ", "),
		headers:          strings.Join(c.AllowedHeaders, ", "),
		exposed:          strings.Join(c.ExposedHeaders, ", "),
		allowed:          make(map[string]struct{}, len(c.AllowedOrigins)),
	}
	for _, o := range c.AllowedOrigins {
		cc.allowed[o] = struct{}{}
	}
	return cc
}

// applyCompiledCORS contains the per-request CORS logic shared by both CORS
// and CORSReloadable.
func applyCompiledCORS(w http.ResponseWriter, r *http.Request, cc *compiledCORS, next http.Handler) {
	origin := r.Header.Get("Origin")
	_, matched := cc.allowed[origin]

	if matched {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
		if cc.allowCredentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		if r.Method == http.MethodOptions {
			// Preflight: short-circuit with 204 before auth guards.
			if cc.methods != "" {
				w.Header().Set("Access-Control-Allow-Methods", cc.methods)
			}
			if cc.headers != "" {
				w.Header().Set("Access-Control-Allow-Headers", cc.headers)
			}
			w.Header().Set("Access-Control-Max-Age", strconv.Itoa(600))
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Non-preflight matched request: expose headers if configured.
		if cc.exposed != "" {
			w.Header().Set("Access-Control-Expose-Headers", cc.exposed)
		}
	}

	next.ServeHTTP(w, r)
}

// CORS returns middleware that enforces config-driven cross-origin resource
// sharing on the upload routes. When cfg.Enabled is false it is a no-op
// passthrough. When enabled, only origins in cfg.AllowedOrigins receive CORS
// headers (exact-match). Preflight OPTIONS requests are answered with 204
// before reaching auth guards, allowing browsers to pre-flight without a token.
func CORS(cfg config.CORS) func(http.Handler) http.Handler {
	if !cfg.Enabled {
		return func(next http.Handler) http.Handler { return next }
	}
	cc := compile(cfg)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			applyCompiledCORS(w, r, cc, next)
		})
	}
}

// CORSReloadable serves CORS from the live config snapshot, recompiling the
// origin set / header strings only when the snapshot version changes.
func CORSReloadable(store *reload.Store) func(http.Handler) http.Handler {
	var (
		mu          sync.Mutex
		lastVersion atomic.Uint64
		cache       atomic.Pointer[compiledCORS]
	)
	cache.Store(compile(store.Load().Uploads.CORS))
	lastVersion.Store(store.Version())

	get := func() *compiledCORS {
		v := store.Version()
		if v != lastVersion.Load() {
			mu.Lock()
			if v != lastVersion.Load() { // re-check under lock
				cache.Store(compile(store.Load().Uploads.CORS))
				lastVersion.Store(v)
			}
			mu.Unlock()
		}
		return cache.Load()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cc := get()
			if !cc.enabled {
				next.ServeHTTP(w, r)
				return
			}
			applyCompiledCORS(w, r, cc, next)
		})
	}
}
