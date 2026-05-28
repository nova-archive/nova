// Command coordinator runs the Nova single-node coordinator: it opens the
// database, boots the embedded hardened Kubo backend, bootstraps the
// keystore, enforces the startup floor, and serves the HTTP read path.
//
// Configuration is via environment (M3 subset; the operator.yaml loader and
// the setup wizard arrive in later milestones):
//
//	DATABASE_URL              postgres DSN (required)
//	NOVA_MASTER_KEY_<LABEL>   master key hex; NOVA_MASTER_KEY_ACTIVE selects (required)
//	NOVA_LISTEN_ADDR          coordinator bind addr (default ":9000")
//	NOVA_KUBO_REPO            Kubo repo dir (required)
//	IPFS_SWARM_KEY_FILE       swarm key path (required in private mode)
//	NOVA_AUTH_ANONYMOUS       "true" to request anonymous mode (refused in prod builds)
//	NOVA_VERSION              version string for /health (default "dev")
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/pkg/coordinator"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "coordinator: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	anonymous := os.Getenv("NOVA_AUTH_ANONYMOUS") == "true"
	if err := auth.EnforceAnonymousPolicy(anonymous); err != nil {
		return err
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	repo := os.Getenv("NOVA_KUBO_REPO")
	if repo == "" {
		return errors.New("NOVA_KUBO_REPO is required")
	}
	swarm := os.Getenv("IPFS_SWARM_KEY_FILE")
	if swarm == "" {
		return errors.New("IPFS_SWARM_KEY_FILE is required in private mode")
	}
	listen := os.Getenv("NOVA_LISTEN_ADDR")
	if listen == "" {
		listen = ":9000"
	}
	version := os.Getenv("NOVA_VERSION")
	if version == "" {
		version = "dev"
	}

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer pool.Close()

	ks, err := envelope.NewKeystoreFromEnv(pool)
	if err != nil {
		return fmt.Errorf("keystore: %w", err)
	}
	if _, err := ks.Bootstrap(ctx); err != nil {
		return fmt.Errorf("keystore bootstrap: %w", err)
	}

	backend, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath:     repo,
		Mode:         ipfs.ModePrivate,
		SwarmKeyPath: swarm,
		Online:       true,
	})
	if err != nil {
		return fmt.Errorf("ipfs backend: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = backend.Close(shutdownCtx)
	}()

	c, err := coordinator.New(pool, backend, ks, coordinator.Config{
		ListenAddr: listen,
		Version:    version,
		RateLimit:  coordinator.RateLimitConfig{RatePerSec: 50, Burst: 200},
	})
	if err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}

	fmt.Fprintf(os.Stderr, "coordinator: listening on %s\n", listen)
	return c.Run(ctx)
}
