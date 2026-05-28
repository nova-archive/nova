package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/ratelimit"
)

// ServerConfig carries the handlers + knobs the router needs.
type ServerConfig struct {
	Version string
	Blob    *handlers.BlobHandler
	Limiter *ratelimit.Limiter
}

// NewServer assembles the chi router with the M3 middleware stack and the
// read-path routes. Storage-core and product namespaces (/api/v1, /fed/v1,
// /i, /v, /a, /d, /r) are intentionally NOT mounted in M3; chi's default
// NotFound returns 404 for them until their owning milestones add them.
func NewServer(cfg ServerConfig) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recover)
	if cfg.Limiter != nil {
		r.Use(middleware.RateLimit(cfg.Limiter))
	}

	r.Get("/health", handlers.Health(cfg.Version))

	if cfg.Blob != nil {
		r.Get("/blob/{cid}", cfg.Blob.Serve)
		r.Head("/blob/{cid}", cfg.Blob.Head)
	}

	return r
}
