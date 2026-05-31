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
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/jobs"
	"github.com/nova-archive/nova/internal/jobs/kinds"
	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/nova-archive/nova/internal/upload"
	"github.com/nova-archive/nova/pkg/coordinator/product"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// RateLimitConfig tunes the in-process per-IP limiter.
type RateLimitConfig struct {
	RatePerSec float64
	Burst      float64
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

	svc      *storage.Service
	queue    *jobs.Queue
	workers  *jobs.WorkerPool
	hook     *productHook
	products map[string]product.Product
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
	}

	sc := api.ServerConfig{Version: cfg.Version, Limiter: limiter}
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
			uh := handlers.NewUploadHandler(store, svc, sizeOrDefault(cfg.MaxUploadSizeBytes), cfg.RecordSourceIP)
			c.uploadHandler = uh
			sc.Upload = uh
		}
	}
	if pool != nil {
		c.queue = jobs.NewQueue(pool)
		c.workers = jobs.NewWorkerPool(c.queue, jobs.WorkerOptions{})
	}

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
	p.RegisterRoutes(c.mux)
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
	if c.uploadStore != nil {
		go c.gcLoop(ctx)
	}
	if c.workers != nil {
		c.workers.RegisterHandler(kinds.KindDerivativePrewarm, kinds.NewDerivativePrewarmHandler(c.prewarm))
		go c.workers.Run(ctx)
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

// gcLoop periodically reclaims abandoned upload sessions until ctx is done.
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
			_, _ = c.uploadStore.GC(ctx)
		}
	}
}
