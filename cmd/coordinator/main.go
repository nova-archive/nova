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
//	NOVA_TRUSTED_PROXIES      comma-separated IPs or CIDRs trusted to set XFF (default empty = XFF ignored)
//	NOVA_OIDC_SIGNING_KEY[_FILE]  Ed25519 seed hex (32 bytes); required in local auth mode
//	NOVA_AUTH_ISSUER_URL      external OIDC issuer URL; empty ⇒ built-in local issuer
//	NOVA_AUTH_CLIENT_ID       external OIDC client id (audience)
//	NOVA_AUTH_ISSUER          local-mode token iss/aud base (default "https://<hostname>/")
//	NOVA_PUBLIC_UPLOADS       "true" allows anonymous uploads (requires NOVA_TOS_URL; T1.20)
//	NOVA_TOS_URL              ToS URL; required when NOVA_PUBLIC_UPLOADS=true
//	NOVA_SIGNED_URL_GRACE_SECONDS               signing-key rotation grace (default 86400)
//	NOVA_SIGNED_URL_REVOCATION_REFRESH_SECONDS  revocation cache refresh (default 30)
//	NOVA_SIGNED_URL_KEY_CACHE_TTL_SECONDS       unwrapped signing-key cache TTL (default 60)
//	NOVA_SIGNED_URL_MAX_TTL_SECONDS             minted-URL ttl cap (default 86400)
//	NOVA_INTEGRITY_AUDIT_ENABLED  "false" disables the M8 integrity-audit scheduler (default enabled)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/audit/integrity"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/localissuer"
	"github.com/nova-archive/nova/internal/auth/oidc"
	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/auth/token"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/db/gen"
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

	trustedProxies, err := httputil.ParseTrustedProxies(os.Getenv("NOVA_TRUSTED_PROXIES"))
	if err != nil {
		return fmt.Errorf("NOVA_TRUSTED_PROXIES: %w", err)
	}

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
	// Ensure an active signing key exists so signed URLs verify and mint out of
	// the box (idempotent; mirrors the master-key bootstrap). M7.
	if err := signedurl.EnsureActiveKey(ctx, gen.New(pool), ks); err != nil {
		return fmt.Errorf("signing key bootstrap: %w", err)
	}

	// Build auth (and enforce its refuse-to-start floors) before the expensive
	// embedded-Kubo boot, so a missing signing key / T1.20 violation fails fast.
	authCfg, err := buildAuthConfig(ctx, gen.New(pool))
	if err != nil {
		return err
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
		TrustedProxies:        trustedProxies,
		Auth:                  authCfg,
		SignedURLs: coordinator.SignedURLConfig{
			Grace:             time.Duration(envInt("NOVA_SIGNED_URL_GRACE_SECONDS", 86400)) * time.Second,
			RevocationRefresh: time.Duration(envInt("NOVA_SIGNED_URL_REVOCATION_REFRESH_SECONDS", 30)) * time.Second,
			KeyCacheTTL:       time.Duration(envInt("NOVA_SIGNED_URL_KEY_CACHE_TTL_SECONDS", 60)) * time.Second,
			MaxTTL:            time.Duration(envInt("NOVA_SIGNED_URL_MAX_TTL_SECONDS", 86400)) * time.Second,
		},
		IntegrityAudit: coordinator.IntegrityAuditConfig{
			Enabled:       os.Getenv("NOVA_INTEGRITY_AUDIT_ENABLED") != "false",
			Cadences:      integrity.DefaultCadences(),
			PassRetention: 30 * 24 * time.Hour,
			FailRetention: 365 * 24 * time.Hour,
		},
	})
	if err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}

	img := imageproduct.New(imgCfg, c.Storage(), pool, c.Queue())
	if err := c.RegisterProduct(img); err != nil {
		return fmt.Errorf("register image product: %w", err)
	}

	// M6.2 D2 — one structured startup line so operators (and log
	// aggregators) can confirm the auth mode, key sources, and listen
	// surface at boot without grepping per-component init lines. No
	// secret values are logged: only the labels and counts that let an
	// operator answer "did this process boot with the config I expect?"
	slog.Info("coordinator startup",
		"mode", authCfg.Descriptor.Mode,
		"issuer", authCfg.Descriptor.IssuerURL,
		"verifier_count", len(authCfg.Verifiers),
		"active_master_key_label", ks.ActiveLabel(),
		"kubo_repo", repo,
		"listen", listen,
		"version", version,
		"public_uploads", authCfg.PublicUploads,
		"paranoid", !recordIP,
		"trusted_proxies", len(trustedProxies),
	)
	return c.Run(ctx)
}

// buildAuthConfig assembles the coordinator's auth wiring from the environment.
// When NOVA_AUTH_ISSUER_URL is set the coordinator runs in external-OIDC mode
// (verify-only; local issuer endpoints 404). Otherwise it runs the built-in
// local Ed25519 issuer, which requires NOVA_OIDC_SIGNING_KEY (refuse-to-start
// floor). Public uploads require NOVA_TOS_URL (T1.20).
func buildAuthConfig(ctx context.Context, q *gen.Queries) (coordinator.AuthConfig, error) {
	var ac coordinator.AuthConfig

	publicUploads := os.Getenv("NOVA_PUBLIC_UPLOADS") == "true"
	if publicUploads && os.Getenv("NOVA_TOS_URL") == "" {
		return ac, errors.New("NOVA_PUBLIC_UPLOADS=true requires NOVA_TOS_URL (T1.20); refusing to start")
	}
	ac.PublicUploads = publicUploads
	// Strict per-IP limiter on /api/v1/auth/login: ~5 attempts/minute, burst 5.
	ac.LoginRate = coordinator.RateLimitConfig{RatePerSec: 5.0 / 60.0, Burst: 5}

	if issuerURL := os.Getenv("NOVA_AUTH_ISSUER_URL"); issuerURL != "" {
		// External-OIDC mode: verify-only. New is resilient to IdP downtime
		// (background discovery retry) and only errors on invalid config.
		clientID := os.Getenv("NOVA_AUTH_CLIENT_ID")
		if clientID == "" {
			// go-oidc requires the client id as the expected token audience;
			// with it empty every token fails verification (universal 401).
			// Fail fast instead of shipping a silently-broken auth surface.
			return ac, errors.New("NOVA_AUTH_CLIENT_ID is required in external OIDC mode (it is the token audience); refusing to start")
		}
		ver, err := oidc.New(ctx, oidc.Config{
			IssuerURL: issuerURL,
			ClientID:  clientID,
			RoleClaim: "groups",
			RoleMapping: map[string]string{
				"nova:operator":  "operator",
				"nova:moderator": "moderator",
				"nova:uploader":  "uploader",
			},
		})
		if err != nil {
			return ac, fmt.Errorf("external oidc: %w", err)
		}
		ac.Verifiers = []auth.Verifier{ver}
		ac.Issuer = nil
		ac.Descriptor = api.AuthConfigDescriptor{Mode: "external", IssuerURL: issuerURL, ClientID: clientID}
		return ac, nil
	}

	// Local-issuer mode: load the Ed25519 signing key (refuse to start if absent).
	seed, signerSrc, err := config.ResolveSecret("NOVA_OIDC_SIGNING_KEY", "NOVA_OIDC_SIGNING_KEY_FILE", "/run/secrets/oidc-signing-key")
	if err != nil || strings.TrimSpace(seed) == "" {
		return ac, errors.New("NOVA_OIDC_SIGNING_KEY is required in local auth mode " +
			"(or set NOVA_AUTH_ISSUER_URL for external OIDC); refusing to start")
	}
	slog.Info("oidc: signing key loaded", "source", string(signerSrc))
	signer, err := token.NewSignerFromSeed(strings.TrimSpace(seed))
	if err != nil {
		return ac, fmt.Errorf("oidc signing key: %w", err)
	}
	issuerURL := os.Getenv("NOVA_AUTH_ISSUER")
	if issuerURL == "" {
		host, herr := os.Hostname()
		if herr != nil || host == "" {
			host = "localhost"
		}
		issuerURL = "https://" + host + "/"
	}
	localIss, err := localissuer.New(localissuer.Config{
		Queries:    q,
		Signer:     signer,
		Gate:       password.NewGate(gateSize()),
		IssuerURL:  issuerURL,
		Audience:   "nova",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 12 * time.Hour,
	})
	if err != nil {
		return ac, fmt.Errorf("local issuer: %w", err)
	}
	ac.Verifiers = []auth.Verifier{localIss.Verifier()}
	ac.Issuer = localIss
	ac.Descriptor = api.AuthConfigDescriptor{Mode: "local"}
	return ac, nil
}

// gateSize bounds concurrent argon2 password verifications to protect against
// login-flood memory exhaustion: min(NumCPU, 4), at least 1.
func gateSize() int {
	n := runtime.NumCPU()
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return n
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
