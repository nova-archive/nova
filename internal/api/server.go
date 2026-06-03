package api

import (
	"encoding/json"
	"net/http"
	"net/netip"

	"github.com/go-chi/chi/v5"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/bearer"
	"github.com/nova-archive/nova/internal/ratelimit"
)

// IssuerHandlers is the subset of localissuer.Issuer methods the server needs.
// The coordinator passes a concrete *localissuer.Issuer which satisfies this
// interface structurally, keeping internal/api free of an import cycle.
type IssuerHandlers interface {
	Login(http.ResponseWriter, *http.Request)
	Refresh(http.ResponseWriter, *http.Request)
	Logout(http.ResponseWriter, *http.Request)
	JWKS(http.ResponseWriter, *http.Request)
}

// AuthConfigDescriptor describes the auth configuration served at
// GET /api/v1/auth/config regardless of mode.
type AuthConfigDescriptor struct {
	Mode      string   `json:"mode"`
	IssuerURL string   `json:"issuer_url,omitempty"`
	ClientID  string   `json:"client_id,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}

// ServerConfig carries the handlers + knobs the router needs.
type ServerConfig struct {
	Version         string
	Blob            *handlers.BlobHandler
	Upload          *handlers.UploadHandler
	Limiter         *ratelimit.Limiter
	Verifiers       []auth.Verifier
	Issuer          IssuerHandlers       // nil => external mode (auth/* 404 except config)
	AuthConfig      AuthConfigDescriptor // always served at /api/v1/auth/config
	Me              *handlers.MeHandler
	PublicUploads   bool
	LoginLimiter    *ratelimit.Limiter
	TrustedProxies  []netip.Prefix                   // gates XFF trust for rate-limit + source-IP recording
	Ready           *handlers.ReadyHandler           // nil ⇒ /readyz returns 200 with no checks
	SignedURLGuard  func(http.Handler) http.Handler  // nil ⇒ no signed-URL verification on reads
	SigningAdmin    *handlers.SigningAdminHandler    // nil ⇒ signed-URL admin endpoints 404
	AuditAdmin      *handlers.AuditAdminHandler      // nil ⇒ integrity-audit listing 404
	ModerationAdmin *handlers.ModerationAdminHandler // nil ⇒ /api/v1/admin/moderation/* 404
	AuditLogAdmin   *handlers.AuditLogAdminHandler   // nil ⇒ /api/v1/admin/audit-log 404
	DMCAIntake      *handlers.DMCAIntakeHandler      // nil ⇒ public /legal/dmca unmounted
}

// NewServer assembles the chi router with the M3 middleware stack and the
// read-path routes. It mounts /health and /blob/{cid} as public (no auth),
// then groups all /api/v1/* routes under bearer.Optional identity hydration.
// Auth issuer endpoints (/api/v1/auth/*) are mounted in local mode; in
// external mode those four paths return 404 external_oidc_active.
// /api/v1/auth/config is always available. /api/v1/admin/* is guarded by
// RequireRole("operator","moderator"). /api/v1/users/me is guarded by
// RequireAuthenticated. Upload write paths are optionally guarded by role.
func NewServer(cfg ServerConfig) *chi.Mux {
	r := chi.NewMux()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recover)
	if cfg.Limiter != nil {
		r.Use(middleware.RateLimit(cfg.Limiter, cfg.TrustedProxies))
	}

	r.Get("/health", handlers.Health(cfg.Version))
	if cfg.Ready != nil {
		r.Get("/readyz", cfg.Ready.Serve)
	}

	// Signed-URL Guard for content reads: when a request carries signed-URL
	// params it is verified (granting private-read authorization on success, or
	// 403 on failure); otherwise it passes straight through. M7.
	readGuard := cfg.SignedURLGuard
	if readGuard == nil {
		readGuard = func(next http.Handler) http.Handler { return next }
	}

	if cfg.Blob != nil {
		r.With(readGuard).Get("/blob/{cid}", cfg.Blob.Serve)
		r.With(readGuard).Head("/blob/{cid}", cfg.Blob.Head)
	}

	// Public DMCA intake (M9): records a case for the operator to review; takes
	// no action. Rate-limited by the global limiter applied above.
	if cfg.DMCAIntake != nil {
		r.Post("/legal/dmca", cfg.DMCAIntake.Submit)
	}

	r.Route("/api/v1", func(r chi.Router) {
		// Auth config is always available, both modes.
		r.Get("/auth/config", authConfigHandler(cfg.AuthConfig))

		if cfg.Issuer != nil {
			// Local mode: mount issuer endpoints.
			if cfg.LoginLimiter != nil {
				r.Group(func(r chi.Router) {
					r.Use(middleware.RateLimit(cfg.LoginLimiter, cfg.TrustedProxies))
					r.Post("/auth/login", cfg.Issuer.Login)
				})
			} else {
				r.Post("/auth/login", cfg.Issuer.Login)
			}
			r.Post("/auth/refresh", cfg.Issuer.Refresh)
			r.Post("/auth/logout", cfg.Issuer.Logout)
			r.Get("/auth/jwks.json", cfg.Issuer.JWKS)
		} else {
			// External mode: those four paths return 404 external_oidc_active.
			r.Post("/auth/login", externalOIDCActive)
			r.Post("/auth/refresh", externalOIDCActive)
			r.Post("/auth/logout", externalOIDCActive)
			r.Get("/auth/jwks.json", externalOIDCActive)
		}

		// Identity-aware group: Optional hydrates identity; guards enforce.
		r.Group(func(r chi.Router) {
			r.Use(bearer.Optional(cfg.Verifiers))

			if cfg.Me != nil {
				r.With(bearer.RequireAuthenticated).Get("/users/me", cfg.Me.Get)
			} else {
				r.With(bearer.RequireAuthenticated).Get("/users/me", func(w http.ResponseWriter, r *http.Request) {
					httputil.WriteError(w, http.StatusNotFound, "not_found", "no such endpoint", middleware.RequestIDFromContext(r.Context()))
				})
			}

			// Admin boundary: guard runs on every matched /admin/* request.
			// The wildcard ensures the route matches, so RequireRole runs
			// before reaching adminNotFound (401 without token, 403 wrong role).
			r.Route("/admin", func(r chi.Router) {
				r.Use(bearer.RequireRole("operator", "moderator"))
				if cfg.SigningAdmin != nil {
					// Key rotation is operator-only; revoke + sign keep the
					// group's operator+moderator guard (moderators run takedowns
					// and hand out shares). M7.
					r.With(bearer.RequireRole("operator")).Post("/keys/rotate-signing", cfg.SigningAdmin.RotateSigning)
					r.Post("/signed-urls/revoke", cfg.SigningAdmin.RevokeSignedURL)
					r.Post("/signed-urls/sign", cfg.SigningAdmin.SignSignedURL)
				}
				// Integrity-audit listing (M8); read-only, operator+moderator.
				if cfg.AuditAdmin != nil {
					r.Get("/audits/integrity", cfg.AuditAdmin.List)
				}
				// Moderation (M9): takedown actions + queue + DMCA cases + blocklist.
				// All operator+moderator (moderators run takedowns per the user_role
				// enum) except clear-legal-hold, which is operator-only.
				if cfg.ModerationAdmin != nil {
					r.Post("/moderation/quarantine", cfg.ModerationAdmin.Quarantine)
					r.Post("/moderation/takedown", cfg.ModerationAdmin.Takedown)
					r.With(bearer.RequireRole("operator")).Post("/moderation/clear-legal-hold", cfg.ModerationAdmin.ClearLegalHold)
					r.Post("/moderation/restore", cfg.ModerationAdmin.Restore)
					r.Post("/moderation/counter-notice", cfg.ModerationAdmin.CounterNotice)
					r.Get("/moderation/queue", cfg.ModerationAdmin.Queue)
					r.Get("/moderation/dmca", cfg.ModerationAdmin.DMCAList)
					r.Get("/moderation/dmca/{id}", cfg.ModerationAdmin.DMCAGet)
					r.Get("/moderation/blocklist", cfg.ModerationAdmin.BlocklistList)
					r.Post("/moderation/blocklist", cfg.ModerationAdmin.BlocklistAdd)
					r.Delete("/moderation/blocklist/{cid}", cfg.ModerationAdmin.BlocklistRemove)
				}
				// Audit-log listing (M9); read-only, operator+moderator.
				if cfg.AuditLogAdmin != nil {
					r.Get("/audit-log", cfg.AuditLogAdmin.List)
				}
				r.Handle("/*", http.HandlerFunc(adminNotFound))
			})

			// Write path (uploads, blobs, images).
			if cfg.Upload != nil {
				mountUploads := func(r chi.Router) {
					r.Route("/uploads", func(r chi.Router) {
						r.Post("/", cfg.Upload.CreateTus)
						r.Route("/{id}", func(r chi.Router) {
							r.Head("/", cfg.Upload.HeadTus)
							r.Patch("/", cfg.Upload.PatchTus)
							r.Delete("/", cfg.Upload.DeleteTus)
							r.Post("/finalize", cfg.Upload.FinalizeTus)
						})
					})
					r.Post("/blobs", cfg.Upload.Multipart)
					r.Post("/images", cfg.Upload.MultipartImage)
				}

				if cfg.PublicUploads {
					r.Group(mountUploads)
				} else {
					r.Group(func(r chi.Router) {
						r.Use(bearer.RequireRole("uploader", "moderator", "operator"))
						mountUploads(r)
					})
				}
			}
		})
	})

	return r
}

// authConfigHandler returns the auth configuration as JSON with Cache-Control: no-store.
func authConfigHandler(d AuthConfigDescriptor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(d)
	}
}

// externalOIDCActive returns 404 external_oidc_active when the local issuer is
// disabled and the caller hits a local-only auth endpoint.
func externalOIDCActive(w http.ResponseWriter, r *http.Request) {
	httputil.WriteError(w, http.StatusNotFound, "external_oidc_active",
		"local issuer disabled; use the configured OIDC provider",
		middleware.RequestIDFromContext(r.Context()))
}

// adminNotFound is reached only after the role guard passes; it returns 404
// for admin endpoints that do not yet exist (M7–M10 will add them).
func adminNotFound(w http.ResponseWriter, r *http.Request) {
	httputil.WriteError(w, http.StatusNotFound, "not_found",
		"no such admin endpoint", middleware.RequestIDFromContext(r.Context()))
}
