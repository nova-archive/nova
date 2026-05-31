package api

import (
	"github.com/go-chi/chi/v5"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/ratelimit"
)

// ServerConfig carries the handlers + knobs the router needs.
type ServerConfig struct {
	Version string
	Blob    *handlers.BlobHandler
	Upload  *handlers.UploadHandler
	Limiter *ratelimit.Limiter
}

// NewServer assembles the chi router with the M3 middleware stack and the
// read-path routes. Storage-core and product namespaces (/api/v1, /fed/v1,
// /i, /v, /a, /d, /r) are intentionally NOT mounted in M3; chi's default
// NotFound returns 404 for them until their owning milestones add them.
func NewServer(cfg ServerConfig) *chi.Mux {
	r := chi.NewMux()
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

	// M4 write path. The rest of /api/v1/* stays 404 until M6.
	if cfg.Upload != nil {
		r.Route("/api/v1/uploads", func(r chi.Router) {
			r.Post("/", cfg.Upload.CreateTus)
			r.Route("/{id}", func(r chi.Router) {
				r.Head("/", cfg.Upload.HeadTus)
				r.Patch("/", cfg.Upload.PatchTus)
				r.Delete("/", cfg.Upload.DeleteTus)
				r.Post("/finalize", cfg.Upload.FinalizeTus)
			})
		})
		r.Post("/api/v1/blobs", cfg.Upload.Multipart)
		r.Post("/api/v1/images", cfg.Upload.MultipartImage)
	}

	return r
}
