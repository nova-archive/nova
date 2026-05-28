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
}

// Coordinator owns the HTTP server. Build with New; drive with Run/Shutdown.
type Coordinator struct {
	cfg     Config
	handler http.Handler
	srv     *http.Server
	addr    atomic.Value // string
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
	if pool != nil && backend != nil && ks != nil {
		svc := storage.NewService(pool, backend, ks)
		sc.Blob = handlers.NewBlobHandler(svc)
	}
	return &Coordinator{cfg: cfg, handler: api.NewServer(sc)}, nil
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
