// Package coordinator is Nova's public, semver-stable coordinator library.
// It owns the HTTP server and composes the storage read core over injected
// dependencies (a pgx pool, an IPFS backend, and a keystore). Dependency
// construction (env, secrets, Kubo boot) is the caller's responsibility —
// see cmd/coordinator. Product registration arrives in M5.
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/audit/integrity"
	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/jobs"
	"github.com/nova-archive/nova/internal/jobs/kinds"
	"github.com/nova-archive/nova/internal/lifecycle"
	"github.com/nova-archive/nova/internal/masterkey"
	"github.com/nova-archive/nova/internal/moderation"
	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/nova-archive/nova/internal/upload"
	"github.com/nova-archive/nova/pkg/coordinator/product"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// revokedRefreshGrace is how long an explicitly-revoked refresh_token row is
// retained before the GC drops it. 30 days preserves the trail for incident
// forensics (the revoked_at + user_agent fields tell you when an attacker's
// stolen sibling token tripped reuse detection) while keeping the table
// from growing without bound.
const revokedRefreshGrace = 30 * 24 * time.Hour

// signingKeyZeroLen is the byte length a shredded signing-key wrapped_key is
// zeroed to: a 256-bit secret wraps to 72 bytes (24 nonce + 32 ct + 16 tag),
// matching the data_encryption_keys convention. M7.
const signingKeyZeroLen = 72

// RateLimitConfig tunes the in-process per-IP limiter.
type RateLimitConfig struct {
	RatePerSec float64
	Burst      float64
}

// AuthConfig carries the auth dependencies threaded into the HTTP server.
// Verifiers and Issuer are built by cmd/coordinator (Task 15) and passed in;
// the coordinator only forwards them to api.ServerConfig.
type AuthConfig struct {
	Verifiers     []auth.Verifier
	Issuer        api.IssuerHandlers // *localissuer.Issuer in local mode; nil in external
	Descriptor    api.AuthConfigDescriptor
	PublicUploads bool
	LoginRate     RateLimitConfig // strict per-IP limiter for /auth/login
}

// Config holds coordinator settings (not dependencies).
type Config struct {
	ListenAddr string
	Version    string
	RateLimit  RateLimitConfig

	// Upload write-path knobs (M4). When UploadTmpDir is set (and the
	// pool/backend/keystore are present), the tus + multipart routes are
	// mounted and a session-GC ticker runs.
	MaxUploadSizeBytes    int64
	MaxConcurrentAssembly int
	SessionTTL            time.Duration
	UploadTmpDir          string
	UploadGCInterval      time.Duration
	RecordSourceIP        bool

	// Auth carries the M6 auth dependencies. Zero value means no auth
	// (verifiers nil, no local issuer, PublicUploads false).
	Auth AuthConfig

	// TrustedProxies gates X-Forwarded-For trust at the rate-limit and
	// source-IP-recording call sites. Nil/empty means XFF is always
	// ignored (the safe default for direct-exposure deployments). When
	// nginx fronts the coordinator on loopback, set this to the proxy's
	// address (e.g., {127.0.0.1/32, ::1/128}). M6.2 B2.
	TrustedProxies []netip.Prefix

	// SignedURLs tunes the M7 signed-URL verifier/rotation/minting. Zero-valued
	// fields take the documented defaults (24h grace, 30s revocation refresh,
	// 60s key cache, 24h max mint ttl).
	SignedURLs SignedURLConfig

	// IntegrityAudit tunes the M8 audit scheduler + retention. Built only when
	// pool+backend+keystore are present.
	IntegrityAudit IntegrityAuditConfig

	// Moderation tunes the M9 scheduled-tombstone sweep. The admin API, intake,
	// and blocklist are built when pool+backend are present regardless.
	Moderation ModerationConfig

	// MasterKeyRotation tunes the M10 re-wrap worker. Zero-valued fields take the
	// documented defaults (4 workers, 256 ids/batch, 50ms inter-batch pace).
	MasterKeyRotation MasterKeyRotationConfig

	// ContentLifecycle tunes the M11 owner soft-delete sweep (grace + cadence).
	ContentLifecycle ContentLifecycleConfig

	// AdminSPA configures coordinator-served admin SPA static assets (M11). An
	// empty DistDir leaves /admin/* unmounted (the feature-gate posture).
	AdminSPA AdminSPAConfig
}

// SignedURLConfig tunes the M7 signed-URL stack.
type SignedURLConfig struct {
	Grace             time.Duration // rotation grace window
	RevocationRefresh time.Duration // revocation cache refresh interval
	KeyCacheTTL       time.Duration // unwrapped signing-key cache TTL
	MaxTTL            time.Duration // mint ttl cap
}

// IntegrityAuditConfig tunes the M8 integrity-audit scheduler and retention.
// A nil/empty Cadences map takes integrity.DefaultCadences(); non-positive
// retentions take the package defaults (30d passes, 365d failures). Enabled
// gates the scheduler (the Maintainer always runs so partition create-ahead
// and pruning continue even when audits are paused).
type IntegrityAuditConfig struct {
	Enabled       bool
	Cadences      map[integrity.Kind]integrity.Cadence
	PassRetention time.Duration
	FailRetention time.Duration
}

// ModerationConfig tunes the M9 scheduled-tombstone sweep. SweepEnabled gates the
// in-process loop (the admin API, intake, and blocklist work regardless); a
// non-positive SweepInterval takes the one-minute default.
type ModerationConfig struct {
	SweepEnabled  bool
	SweepInterval time.Duration
}

// MasterKeyRotationConfig tunes the M10 re-wrap worker (concurrency/batch/pace).
type MasterKeyRotationConfig struct {
	RewrapConcurrency int
	RewrapBatchSize   int
	RewrapPace        time.Duration
}

// ContentLifecycleConfig tunes the M11 owner soft-delete sweep. SweepEnabled
// gates the in-process loop (the owner GET/DELETE blob routes work regardless);
// a non-positive SoftDeleteGrace defaults to 24h and a non-positive SweepInterval
// to one minute.
type ContentLifecycleConfig struct {
	SweepEnabled    bool
	SoftDeleteGrace time.Duration
	SweepInterval   time.Duration
}

// AdminSPAConfig configures coordinator-served admin SPA static assets (M11).
type AdminSPAConfig struct {
	DistDir string // NOVA_ADMIN_DIST_DIR; empty ⇒ /admin/* unmounted
}

// Coordinator owns the HTTP server. Build with New; register products before
// Run; drive with Run/Shutdown.
type Coordinator struct {
	cfg           Config
	mux           *chi.Mux
	srv           *http.Server
	addr          atomic.Value // string
	uploadStore   *upload.Store
	uploadHandler *handlers.UploadHandler
	gcInterval    time.Duration

	svc         *storage.Service
	queue       *jobs.Queue
	workers     *jobs.WorkerPool
	authQueries *gen.Queries // refresh-token GC (nil when no pool)
	hook        *productHook
	products    map[string]product.Product

	// Rate limiters are held for periodic LRU sweep in gcLoop.
	limiter      *ratelimit.Limiter
	loginLimiter *ratelimit.Limiter

	// Signed-URL wiring (M7). Built when pool + keystore are present. The Guard
	// gates the public read paths (/blob, /i/*); revocations is loaded and
	// refreshed in Run.
	signedURLGuard func(http.Handler) http.Handler
	revocations    *signedurl.DBRevocations
	revRefresh     time.Duration

	// Integrity-audit wiring (M8). Built when pool + backend + keystore are
	// present; the scheduler runs in-process (no jobs.Queue) and the maintainer
	// keeps integrity_audits' partitions provisioned + pruned.
	auditScheduler  *integrity.Scheduler
	auditMaintainer *integrity.Maintainer
	auditEnabled    bool

	// Moderation wiring (M9). The in-process Sweeper tombstones overdue
	// quarantines; built when pool + backend are present.
	modSweeper *moderation.Sweeper

	// Owner content-lifecycle wiring (M11). The in-process Sweeper tombstones
	// overdue soft-deletes via the shared lifecycle.TombstoneTree primitive; built
	// when pool + backend are present.
	lifecycleSweeper *lifecycle.Sweeper

	// Master-key rotation wiring (M10). Built when pool + keystore are present;
	// the Rotator re-wraps DEKs and signing keys to the active master-key version.
	masterKeyRotator *masterkey.Rotator
}

// New constructs a coordinator from injected dependencies. pool/backend/ks may
// be nil for tests that only exercise health + lifecycle. When all three are
// present, the blob read + write routes are mounted and product uploads are
// analyzed through the WriteHook. When pool is present, the job queue + worker
// pool are constructed (started in Run). Register products via RegisterProduct
// BEFORE calling Run.
func New(pool *pgxpool.Pool, backend ipfs.Backend, ks *envelope.Keystore, cfg Config) (*Coordinator, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("coordinator: ListenAddr is required")
	}
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		RatePerSec: cfg.RateLimit.RatePerSec, Burst: cfg.RateLimit.Burst,
	}, nil)

	hook := &productHook{products: map[string]product.Product{}}
	c := &Coordinator{
		cfg:        cfg,
		gcInterval: cfg.UploadGCInterval,
		hook:       hook,
		products:   hook.products,
		limiter:    limiter,
	}

	sc := api.ServerConfig{
		Version:        cfg.Version,
		Limiter:        limiter,
		TrustedProxies: cfg.TrustedProxies,
	}
	// auditW is the M9 audit_log writer; built when a pool is present and shared
	// by the moderation stack (atomic WriteTx) and the M7 admin backfill (Write).
	var auditW *auditlog.Writer
	if pool != nil && backend != nil && ks != nil {
		svc := storage.NewService(pool, backend, ks,
			storage.WithWriteLimits(cfg.MaxUploadSizeBytes, cfg.MaxConcurrentAssembly),
			storage.WithProductHook(hook))
		c.svc = svc
		sc.Blob = handlers.NewBlobHandler(svc)

		// Mount the write path only when a chunk tmp dir is configured.
		if cfg.UploadTmpDir != "" {
			store, err := upload.NewStore(pool, svc, cfg.UploadTmpDir, cfg.SessionTTL, sizeOrDefault(cfg.MaxUploadSizeBytes))
			if err != nil {
				return nil, err
			}
			c.uploadStore = store
			uh := handlers.NewUploadHandler(store, svc, sizeOrDefault(cfg.MaxUploadSizeBytes), cfg.RecordSourceIP, cfg.TrustedProxies)
			c.uploadHandler = uh
			sc.Upload = uh
		}
	}
	if pool != nil {
		c.queue = jobs.NewQueue(pool)
		c.workers = jobs.NewWorkerPool(c.queue, jobs.WorkerOptions{})
		c.authQueries = gen.New(pool)
		auditW = auditlog.NewWriter(gen.New(pool), slog.Default())
	}

	sc.Verifiers = cfg.Auth.Verifiers
	sc.Issuer = cfg.Auth.Issuer
	sc.AuthConfig = cfg.Auth.Descriptor
	sc.PublicUploads = cfg.Auth.PublicUploads
	if cfg.Auth.LoginRate.RatePerSec > 0 {
		c.loginLimiter = ratelimit.NewLimiter(ratelimit.Config{
			RatePerSec: cfg.Auth.LoginRate.RatePerSec, Burst: cfg.Auth.LoginRate.Burst,
		}, nil)
		sc.LoginLimiter = c.loginLimiter
	}
	if pool != nil {
		sc.Me = handlers.NewMeHandler(gen.New(pool))
	}

	// Signed-URL verifier + admin handlers (M7). Built when a pool and keystore
	// are present: the Guard gates public reads, and the admin endpoints
	// rotate/revoke/mint signed URLs. Zero-valued knobs take their defaults.
	if pool != nil && ks != nil {
		q := gen.New(pool)
		keyTTL := orDefaultDuration(cfg.SignedURLs.KeyCacheTTL, time.Minute)
		grace := orDefaultDuration(cfg.SignedURLs.Grace, 24*time.Hour)
		maxTTL := orDefaultDuration(cfg.SignedURLs.MaxTTL, 24*time.Hour)
		c.revRefresh = orDefaultDuration(cfg.SignedURLs.RevocationRefresh, 30*time.Second)

		keySource := signedurl.NewKeySource(q, ks, keyTTL)
		c.revocations = signedurl.NewRevocations(q)
		verifier := signedurl.NewVerifier(keySource, c.revocations)
		c.signedURLGuard = verifier.Guard
		sc.SignedURLGuard = verifier.Guard
		sc.SigningAdmin = handlers.NewSigningAdminHandler(pool, ks, keySource, c.revocations, grace, maxTTL)
		if auditW != nil {
			sc.SigningAdmin.SetAuditWriter(auditW) // M9 best-effort operator-action trail
		}

		// Master-key Rotator (M10): re-wraps all DEKs and signing keys from a
		// retiring version to the active one. OnSigningRewrap invalidates the
		// signing-key cache defensively (re-wrap doesn't change the secret, but
		// the version ID changes, so cached entries may become stale).
		mkr := masterkey.NewRotator(masterkey.Config{
			Q: q, Pool: pool, Keystore: ks, Audit: auditW, Logger: slog.Default(),
			Concurrency:     cfg.MasterKeyRotation.RewrapConcurrency,
			BatchSize:       cfg.MasterKeyRotation.RewrapBatchSize,
			Pace:            cfg.MasterKeyRotation.RewrapPace,
			OnSigningRewrap: func() { keySource.Invalidate() },
		})
		c.masterKeyRotator = mkr
		sc.MasterKeyAdmin = handlers.NewMasterKeyAdminHandler(mkr, auditW)
	}

	// Integrity-audit subsystem (M8): the in-process scheduler runs the seven
	// checks on per-kind cadences, the maintainer provisions + prunes the
	// integrity_audits partitions, and the admin handler serves the listing.
	// Built when pool + backend + keystore are present.
	if pool != nil && backend != nil && ks != nil {
		q := gen.New(pool)
		cadences := cfg.IntegrityAudit.Cadences
		if len(cadences) == 0 {
			cadences = integrity.DefaultCadences()
		}
		if err := integrity.EnforceAuditPolicy(cadences); err != nil {
			return nil, fmt.Errorf("coordinator: integrity audit: %w", err)
		}
		rec := integrity.NewRecorder(q, integrity.NewNoopSink(), slog.Default())
		c.auditScheduler = integrity.NewScheduler(integrity.NewChecks(q, backend, ks), cadences, rec, q, slog.Default())
		c.auditMaintainer = integrity.NewMaintainer(pool, cfg.IntegrityAudit.PassRetention, cfg.IntegrityAudit.FailRetention, slog.Default())
		c.auditEnabled = cfg.IntegrityAudit.Enabled
		sc.AuditAdmin = handlers.NewAuditAdminHandler(q)
	}

	// Moderation subsystem (M9): the Service runs quarantine / tombstone /
	// clear-legal-hold / restore / counter-notice transactions (auditing each
	// in-tx via auditW); the Sweeper tombstones overdue quarantines; the admin +
	// public handlers expose the surface. Built when pool + backend are present
	// (tombstone shreds DEKs and unpins). hook.OnDelete is the derivative-state
	// cascade seam; products registered later are dispatched at call time.
	if pool != nil && backend != nil {
		q := gen.New(pool)
		modSvc := moderation.NewService(q, pool, backend, hook.OnDelete, auditW, slog.Default(), time.Now)
		c.modSweeper = moderation.NewSweeper(modSvc, cfg.Moderation.SweepInterval, cfg.Moderation.SweepEnabled, slog.Default())
		sc.ModerationAdmin = handlers.NewModerationAdminHandler(modSvc, q)
		sc.DMCAIntake = handlers.NewDMCAIntakeHandler(q)
		sc.AuditLogAdmin = handlers.NewAuditLogAdminHandler(q)
	}

	// Owner content-lifecycle (M11): SoftDelete flips active→soft_deleted; the
	// Sweeper tombstones overdue soft-deletes via the shared lifecycle.TombstoneTree
	// primitive (the same crypto-shred/unpin as moderation) with its own blob.*
	// audit vocabulary. hook.OnDelete is the shared derivative cascade. The owner
	// blob routes + the admin blob/jobs listings are mounted alongside.
	if pool != nil && backend != nil {
		q := gen.New(pool)
		lifeSvc := lifecycle.NewService(q, pool, backend, hook.OnDelete, auditW, slog.Default(), time.Now, cfg.ContentLifecycle.SoftDeleteGrace)
		c.lifecycleSweeper = lifecycle.NewSweeper(lifeSvc, cfg.ContentLifecycle.SweepInterval, cfg.ContentLifecycle.SweepEnabled, slog.Default())
		sc.BlobMeta = handlers.NewBlobMetaHandler(q, lifeSvc)
		sc.BlobsAdmin = handlers.NewBlobsAdminHandler(q)
		sc.JobsAdmin = handlers.NewJobsAdminHandler(jobs.NewAdminStore(pool))
	}

	// Admin SPA static serving (M11): gated by NOVA_ADMIN_DIST_DIR (nil ⇒ /admin/*
	// unmounted). Independent of pool/backend. In external-OIDC mode the issuer
	// origin is added to the CSP connect-src so the SPA's authorization-code + PKCE
	// token exchange can reach the operator's IdP.
	var spaConnect []string
	if cfg.Auth.Descriptor.Mode == "external" && cfg.Auth.Descriptor.IssuerURL != "" {
		if u, perr := url.Parse(cfg.Auth.Descriptor.IssuerURL); perr == nil && u.Scheme != "" && u.Host != "" {
			spaConnect = append(spaConnect, u.Scheme+"://"+u.Host)
		}
	}
	sc.AdminSPA = handlers.NewAdminSPA(cfg.AdminSPA.DistDir, spaConnect...)

	// /readyz checks. Each is a thin wrapper over the corresponding dep's
	// liveness probe; the handler runs them in parallel under a 1 s deadline.
	// Only checks for present deps are registered, so a no-pool / no-backend
	// test coordinator still serves /readyz coherently (the empty-checks
	// case returns 200 — matches /health's process-alive semantics).
	var ready []handlers.ReadyCheck
	if pool != nil {
		pool := pool
		ready = append(ready, handlers.ReadyCheck{
			Name: "database",
			Fn:   func(ctx context.Context) error { return pool.Ping(ctx) },
		})
	}
	// Signing-key readiness: at least one active signing key must exist for
	// signed-URL verification/minting (auto-bootstrapped at startup). M7.
	if pool != nil {
		q := gen.New(pool)
		ready = append(ready, handlers.ReadyCheck{
			Name: "signing_keys",
			Fn: func(ctx context.Context) error {
				n, err := q.CountActiveSigningKeys(ctx)
				if err != nil {
					return err
				}
				if n == 0 {
					return errors.New("no active signing key")
				}
				return nil
			},
		})
	}
	// Master-key rotation stall detection (M10): degrades when a rotation is in
	// progress but the source key is not loaded.
	if c.masterKeyRotator != nil {
		mkr := c.masterKeyRotator
		ready = append(ready, handlers.ReadyCheck{
			Name: "master_key_rotation",
			Fn:   func(ctx context.Context) error { return mkr.Readyz(ctx) },
		})
	}
	if backend != nil {
		backend := backend
		ready = append(ready, handlers.ReadyCheck{
			Name: "ipfs",
			Fn:   func(ctx context.Context) error { return backend.Health(ctx) },
		})
	}
	// Verifier readiness — only verifiers that opt into ReadinessChecker
	// (oidc.Verifier today) report a readiness signal. The localissuer
	// verifier is always ready once constructed and skipped.
	for i, v := range cfg.Auth.Verifiers {
		if rc, ok := v.(interface{ Ready() bool }); ok {
			rc := rc
			ready = append(ready, handlers.ReadyCheck{
				Name: fmt.Sprintf("verifier_%d", i),
				Fn: func(ctx context.Context) error {
					if rc.Ready() {
						return nil
					}
					return errors.New("verifier discovery pending")
				},
			})
		}
	}
	sc.Ready = handlers.NewReadyHandler(time.Second, ready...)

	c.mux = api.NewServer(sc)
	return c, nil
}

// sizeOrDefault returns n, or the 100 MiB default when n is non-positive.
func sizeOrDefault(n int64) int64 {
	if n <= 0 {
		return 104857600
	}
	return n
}

// orDefaultDuration returns d, or def when d is non-positive.
func orDefaultDuration(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// reservedPrefixes are namespaces owned by the storage core / coordinator that
// a product MUST NOT mount routes under.
var reservedPrefixes = []string{"/api/v1", "/blob", "/health", "/fed/v1", "/legal"}

// Storage returns the storage Service (nil when constructed without
// pool/backend/keystore). Used by cmd to build product layers.
func (c *Coordinator) Storage() *storage.Service { return c.svc }

// Queue returns the job queue (nil when constructed without a pool). Used by
// cmd to wire product OnCommitted enqueue sites. Phase-1 internal wiring.
func (c *Coordinator) Queue() *jobs.Queue { return c.queue }

// Handler returns the root HTTP handler (useful for in-process tests).
func (c *Coordinator) Handler() http.Handler { return c.mux }

// RegisterProduct mounts a product's routes and enrolls it in the write-hook
// dispatch + prewarm registry. It rejects a product whose routes would collide
// with a reserved namespace. Call before Run.
func (c *Coordinator) RegisterProduct(p product.Product) error {
	// Probe the product's routes on a throwaway router; reject reserved-prefix collisions.
	probe := chi.NewRouter()
	p.RegisterRoutes(probe)
	var collision error
	_ = chi.Walk(probe, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		for _, rp := range reservedPrefixes {
			if route == rp || strings.HasPrefix(route, rp+"/") {
				if collision == nil {
					collision = fmt.Errorf("coordinator: product %q route %s %q collides with reserved prefix %q", p.Name(), method, route, rp)
				}
			}
		}
		return nil
	})
	if collision != nil {
		return collision
	}
	// Mount product read routes (/i/*) behind the signed-URL Guard when it is
	// built, so a valid signed URL unlocks private image content the same way
	// it does for /blob. The Guard passes through when no sig params are present.
	if c.signedURLGuard != nil {
		c.mux.Group(func(r chi.Router) {
			r.Use(c.signedURLGuard)
			p.RegisterRoutes(r)
		})
	} else {
		p.RegisterRoutes(c.mux)
	}
	c.products[p.Name()] = p

	// If this product is image-capable (exposes preset URLs) and the multipart
	// upload edge is mounted, wire its accept-predicate + preset-URL builder so
	// /api/v1/images can do an early 415 and emit urls.presets.
	if c.uploadHandler != nil {
		if edge, ok := p.(interface {
			PresetURLs(cid string) map[string]string
		}); ok {
			accepted := p.AcceptedMimeTypes()
			set := make(map[string]bool, len(accepted))
			for _, m := range accepted {
				set[m] = true
			}
			c.uploadHandler.SetImageHooks(func(mime string) bool { return set[mime] }, edge.PresetURLs)
		}
	}

	return nil
}

// prewarm dispatches a derivative_prewarm job to the image product, if one is
// registered and exposes a Prewarm method. Other/absent products no-op.
func (c *Coordinator) prewarm(ctx context.Context, parentCID string, presets []string) error {
	p, ok := c.products["image"]
	if !ok {
		return nil
	}
	pw, ok := p.(interface {
		Prewarm(context.Context, string, []string) error
	})
	if !ok {
		return nil
	}
	return pw.Prewarm(ctx, parentCID, presets)
}

// Addr returns the actual listen address once Run has bound (useful when
// ListenAddr uses :0). Empty until bound.
func (c *Coordinator) Addr() string {
	if v, ok := c.addr.Load().(string); ok {
		return v
	}
	return ""
}

// Run binds the listener and serves until ctx is cancelled, then drains.
func (c *Coordinator) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", c.cfg.ListenAddr)
	if err != nil {
		return err
	}
	c.addr.Store(ln.Addr().String())
	if c.revocations != nil {
		_ = c.revocations.Load(ctx) // best-effort initial load; refresh retries
		go c.revocations.RefreshEvery(ctx, c.revRefresh)
	}
	if c.uploadStore != nil || c.authQueries != nil {
		go c.gcLoop(ctx)
	}
	if c.workers != nil {
		c.workers.RegisterHandler(kinds.KindDerivativePrewarm, kinds.NewDerivativePrewarmHandler(c.prewarm))
		go c.workers.Run(ctx)
	}
	// Integrity-audit maintainer always runs (partition create-ahead + pruning);
	// the scheduler runs only when audits are enabled. M8.
	if c.auditMaintainer != nil {
		go c.auditMaintainer.Run(ctx)
	}
	if c.auditScheduler != nil && c.auditEnabled {
		go c.auditScheduler.Run(ctx)
	}
	// Scheduled-tombstone sweep (M9); a disabled sweeper's Run returns at once.
	if c.modSweeper != nil {
		go c.modSweeper.Run(ctx)
	}
	// Owner soft-delete sweep (M11); a disabled sweeper's Run returns at once.
	if c.lifecycleSweeper != nil {
		go c.lifecycleSweeper.Run(ctx)
	}
	// Master-key re-wrap worker (M10): resumes any interrupted rotation on boot,
	// then drains on each operator-triggered Start call.
	if c.masterKeyRotator != nil {
		go c.masterKeyRotator.Run(ctx)
	}
	c.srv = &http.Server{Handler: c.mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = c.Shutdown(shutdownCtx)
	}()

	if err := c.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the HTTP server. It does NOT close injected
// dependencies — the caller owns their lifecycle.
func (c *Coordinator) Shutdown(ctx context.Context) error {
	if c.srv == nil {
		return nil
	}
	return c.srv.Shutdown(ctx)
}

// gcLoop periodically reclaims abandoned upload sessions, expired refresh
// tokens, and stale rate-limiter buckets until ctx is done. The sweep
// window for rate-limiter buckets matches the gcInterval (so on the
// default 1 h interval, a bucket idle for 1+ h is evicted on the next
// tick); this is best-effort housekeeping — the Config.MaxKeys cap is
// the hard safety net.
func (c *Coordinator) gcLoop(ctx context.Context) {
	interval := c.gcInterval
	if interval <= 0 {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.uploadStore != nil {
				_, _ = c.uploadStore.GC(ctx)
			}
			if c.authQueries != nil {
				_, _ = c.authQueries.DeleteExpiredRefreshTokens(ctx)
				cutoff := pgtype.Timestamptz{Time: time.Now().Add(-revokedRefreshGrace), Valid: true}
				_, _ = c.authQueries.DeleteRevokedRefreshTokensOlderThan(ctx, cutoff)
				// Crypto-shred signing keys past their grace window. M7.
				_ = c.authQueries.ShredExpiredRetiredSigningKeys(ctx, make([]byte, signingKeyZeroLen))
			}
			if c.limiter != nil {
				c.limiter.Sweep(interval)
			}
			if c.loginLimiter != nil {
				c.loginLimiter.Sweep(interval)
			}
		}
	}
}
