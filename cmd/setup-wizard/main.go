// Command setup-wizard is a thin alias for the coordinator's first-run setup
// server. It opens the database from DATABASE_URL and runs the same /setup
// wizard seam that the coordinator auto-detects via the bootstrap sentinel.
//
// In normal deployments the coordinator binary detects setup mode on its own
// (sentinel absent ⇒ setup branch; sentinel present ⇒ normal mode). This
// binary is the explicit entry-point useful for "docker compose run" or
// manual pre-flight setup without the full coordinator process.
//
// Environment variables (same as cmd/coordinator):
//
//	DATABASE_URL          postgres DSN (required)
//	NOVA_LISTEN_ADDR      bind address (default ":9000")
//	NOVA_VERSION          version string for /health (default "dev")
//	NOVA_CONFIG_DIR       location of operator.yaml + sentinel (default "/etc/nova")
//	NOVA_SECRETS_DIR      location for written secrets (default "/run/secrets")
//	NOVA_SETUP_DIST_DIR   pre-built setup SPA bundle (optional; API works without it)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/setup"
	"github.com/nova-archive/nova/pkg/coordinator"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "setup-wizard: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer pool.Close()

	configDir := os.Getenv("NOVA_CONFIG_DIR")
	if configDir == "" {
		configDir = "/etc/nova"
	}
	secretsDir := os.Getenv("NOVA_SECRETS_DIR")
	if secretsDir == "" {
		secretsDir = "/run/secrets"
	}
	listen := os.Getenv("NOVA_LISTEN_ADDR")
	if listen == "" {
		listen = ":9000"
	}
	version := os.Getenv("NOVA_VERSION")
	if version == "" {
		version = "dev"
	}

	sentinelPath := filepath.Join(configDir, ".bootstrap-complete")

	slog.Info("setup-wizard starting", "listen", listen, "config_dir", configDir)

	setupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	return coordinator.RunSetupServer(setupCtx, coordinator.SetupServerConfig{
		ListenAddr:   listen,
		Version:      version,
		Pool:         pool,
		SetupDistDir: os.Getenv("NOVA_SETUP_DIST_DIR"),
		Paths: setup.Paths{
			ConfigDir:  configDir,
			SecretsDir: secretsDir,
			Sentinel:   sentinelPath,
		},
		AfterCommit: cancel, // commit → cancel → graceful shutdown → process exit
	})
}
