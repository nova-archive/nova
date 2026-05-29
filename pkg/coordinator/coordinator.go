// Package coordinator is Nova's public, semver-stable coordinator library.
// It owns the HTTP server and composes the storage read core over injected
// dependencies (a pgx pool, an IPFS backend, and a keystore). Dependency
// construction (env, secrets, Kubo boot) is the caller's responsibility —
// see cmd/coordinator. Product registration arrives in M5.
package coordinator

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/nova-archive/nova/internal/upload"
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

// Coordinator owns the HTTP server. Build with New; drive with Run/Shutdown.
type Coordinator struct {
	cfg         Config
	handler     http.Handler
	srv         *http.Server
	addr        atomic.Value // string
	uploadStore *upload.Store
	gcInterval  time.Duration
}

// New constructs a coordinator from injected dependencies. pool/backend/ks
// may be nil for tests that only exercise health + lifecycle. When all three
// are present, the blob read routes are mounted.
func New(pool *pgxpool.Pool, backend ipfs.Backend, ks *envelope.Keystore, cfg Config) (*Coordinator, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("coordinator: ListenAddr is required")
	}
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		RatePerSec: cfg.RateLimit.RatePerSec, Burst: cfg.RateLimit.Burst,
	}, nil)

	sc := api.ServerConfig{Version: cfg.Version, Limiter: limiter}
	var uploadStore *upload.Store
	if pool != nil && backend != nil && ks != nil {
		svc := storage.NewService(pool, backend, ks,
			storage.WithWriteLimits(cfg.MaxUploadSizeBytes, cfg.MaxConcurrentAssembly))
		sc.Blob = handlers.NewBlobHandler(svc)

		// Mount the write path only when a chunk tmp dir is configured.
		if cfg.UploadTmpDir != "" {
			store, err := upload.NewStore(pool, svc, cfg.UploadTmpDir, cfg.SessionTTL, sizeOrDefault(cfg.MaxUploadSizeBytes))
			if err != nil {
				return nil, err
			}
			uploadStore = store
			sc.Upload = handlers.NewUploadHandler(store, svc, sizeOrDefault(cfg.MaxUploadSizeBytes), cfg.RecordSourceIP)
		}
	}
	return &Coordinator{
		cfg: cfg, handler: api.NewServer(sc),
		uploadStore: uploadStore, gcInterval: cfg.UploadGCInterval,
	}, nil
}

// sizeOrDefault returns n, or the 100 MiB default when n is non-positive.
func sizeOrDefault(n int64) int64 {
	if n <= 0 {
		return 104857600
	}
	return n
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
	c.srv = &http.Server{Handler: c.handler, ReadHeaderTimeout: 10 * time.Second}

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
