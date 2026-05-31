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
//	NOVA_UPLOAD_TMP_DIR       tus chunk dir (default <tmpdir>/nova-uploads)
//	NOVA_MAX_UPLOAD_SIZE_BYTES   max upload size (default 100 MiB)
//	NOVA_MAX_CONCURRENT_ASSEMBLY concurrent in-memory encrypts (default 8)
//	NOVA_UPLOAD_SESSION_TTL_SECONDS  tus session TTL (default 86400)
//	NOVA_PARANOID             "true" suppresses source-IP recording
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	novaimage "github.com/nova-archive/nova/nova-image"
	"github.com/nova-archive/nova/nova-image/imageproduct"
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

	tmpDir := os.Getenv("NOVA_UPLOAD_TMP_DIR")
	if tmpDir == "" {
		tmpDir = filepath.Join(os.TempDir(), "nova-uploads")
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return fmt.Errorf("create upload tmp dir: %w", err)
	}
	maxUpload := envInt64("NOVA_MAX_UPLOAD_SIZE_BYTES", config.DefaultMaxUploadSizeBytes)
	maxAssembly := envInt("NOVA_MAX_CONCURRENT_ASSEMBLY", config.DefaultMaxConcurrentAssembly)
	sessionTTL := time.Duration(envInt("NOVA_UPLOAD_SESSION_TTL_SECONDS", config.DefaultUploadSessionTTLSecs)) * time.Second
	recordIP := os.Getenv("NOVA_PARANOID") != "true"

	// Image product config. Phase-1: defaults only (operator.yaml image-section
	// decode is deferred until the operator.yaml loader is wired into cmd).
	imgCfg := novaimage.DefaultConfig()
	if err := imgCfg.Validate(); err != nil {
		return fmt.Errorf("image config: %w", err)
	}
	if err := imageproduct.Startup(imgCfg.VipsCacheMaxMemBytes); err != nil {
		return fmt.Errorf("libvips startup: %w", err)
	}
	if err := imageproduct.ValidateCodecs(imgCfg.AllowedInputFormats, imgCfg.AllowedOutputFormats); err != nil {
		return fmt.Errorf("image codec unavailable (refusing to start): %w", err)
	}
	if imgCfg.FormatConversion.Enabled && !imgCfg.FormatConversion.Lossless {
		fmt.Fprintf(os.Stderr, "coordinator: NOTICE: format_conversion is enabled with lossless=false; "+
			"uploaded lossless images (PNG/BMP/TIFF) will be re-encoded to %s with quality loss (destructive)\n",
			imgCfg.FormatConversion.Target)
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
		ListenAddr:            listen,
		Version:               version,
		RateLimit:             coordinator.RateLimitConfig{RatePerSec: 50, Burst: 200},
		MaxUploadSizeBytes:    maxUpload,
		MaxConcurrentAssembly: maxAssembly,
		SessionTTL:            sessionTTL,
		UploadTmpDir:          tmpDir,
		UploadGCInterval:      time.Hour,
		RecordSourceIP:        recordIP,
	})
	if err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}

	img := imageproduct.New(imgCfg, c.Storage(), pool, c.Queue())
	if err := c.RegisterProduct(img); err != nil {
		return fmt.Errorf("register image product: %w", err)
	}

	fmt.Fprintf(os.Stderr, "coordinator: listening on %s\n", listen)
	return c.Run(ctx)
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
