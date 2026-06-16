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
//	NOVA_MODERATION_SWEEP_ENABLED "false" disables the M9 scheduled-tombstone sweep (default enabled)
//	NOVA_MASTER_KEY_REWRAP_CONCURRENCY   M10 re-wrap worker goroutines (default 4)
//	NOVA_MASTER_KEY_REWRAP_BATCH         M10 re-wrap ids claimed per tx (default 256)
//	NOVA_MASTER_KEY_REWRAP_PACE_MS       M10 re-wrap inter-batch pace ms (default 50)
//	NOVA_ADMIN_DIST_DIR                  M11 admin SPA bundle dir served at /admin/* (unset ⇒ disabled)
//	NOVA_WIDGET_DIST_DIR                 M12 upload-widget bundle dir served at /widget/* (unset ⇒ disabled)
//	NOVA_SOFT_DELETE_GRACE_SECONDS       M11 owner soft-delete grace before tombstone+shred (default 86400)
//	NOVA_LIFECYCLE_SWEEP_INTERVAL_MS     M11 owner soft-delete sweep cadence ms (default 60000)
//	NOVA_SOFT_DELETE_SWEEP_ENABLED       "false" disables the M11 soft-delete sweep (default enabled)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	"github.com/nova-archive/nova/internal/auth/uploadtoken"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/config/reload"
	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
	fedcoord "github.com/nova-archive/nova/internal/federation/coordinator"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/secret"
	"github.com/nova-archive/nova/internal/setup"
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

	opCfg, err := loadOperatorConfigFile()
	if err != nil {
		return fmt.Errorf("operator.yaml: %w", err)
	}
	rc := resolveOperatorConfig(opCfg, os.Getenv)

	// SETUP MODE: when the bootstrap sentinel is absent, serve only the /setup
	// wizard seam. No keystore/Kubo/auth is booted — the master key does not
	// exist until the wizard commits. On a successful commit, AfterCommit (cancel)
	// causes a clean shutdown; the process supervisor restarts in normal mode.
	configDir := os.Getenv("NOVA_CONFIG_DIR")
	if configDir == "" {
		configDir = "/etc/nova"
	}
	sentinelPath := filepath.Join(configDir, ".bootstrap-complete")
	if _, statErr := os.Stat(sentinelPath); errors.Is(statErr, os.ErrNotExist) {
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			return errors.New("DATABASE_URL is required")
		}
		pool, err := db.Open(ctx, dsn)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer pool.Close()

		listen := os.Getenv("NOVA_LISTEN_ADDR")
		if listen == "" {
			listen = ":9000"
		}
		version := os.Getenv("NOVA_VERSION")
		if version == "" {
			version = "dev"
		}
		secretsDir := os.Getenv("NOVA_SECRETS_DIR")
		if secretsDir == "" {
			secretsDir = "/run/secrets"
		}

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
			AfterCommit: cancel, // commit → cancel → graceful shutdown → process exit → restart in normal mode
		})
	}
	// NORMAL MODE: full boot below (unchanged).

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
	maxUpload := rc.MaxUploadSizeBytes
	maxAssembly := rc.MaxConcurrentAssembly
	sessionTTL := time.Duration(envInt("NOVA_UPLOAD_SESSION_TTL_SECONDS", config.DefaultUploadSessionTTLSecs)) * time.Second
	// Source-IP recording: an explicit operator.yaml record_source_ip wins;
	// otherwise it follows the privacy preset (paranoid ⇒ off). The paranoid
	// preset (config.ApplyPrivacyPreset) fills record_source_ip when unset, so a
	// paranoid operator.yaml already arrives here non-nil. P2-M0.2.
	recordIP := !rc.Paranoid
	if opCfg != nil && opCfg.Coordinator.RecordSourceIP != nil {
		recordIP = *opCfg.Coordinator.RecordSourceIP
	}

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
	authCfg, err := buildAuthConfig(ctx, gen.New(pool), rc)
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

	// Live config store (P2-M0.4): built only when an operator.yaml exists so the
	// nil-store invariant holds in pure-env mode. reload.New applies the env
	// overlay onto opCfg in-place (desirable — the running coordinator's other
	// reads of opCfg/rc already incorporate env).
	cfgPath := operatorConfigPath()
	var cfgStore *reload.Store
	if opCfg != nil && cfgPath != "" {
		cfgStore = reload.New(opCfg, func(c *config.Config) { applyEnvOverridesTo(c, os.Getenv) }, envPinnedKeys(os.Getenv))
	}

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
		Moderation: coordinator.ModerationConfig{
			SweepEnabled:  os.Getenv("NOVA_MODERATION_SWEEP_ENABLED") != "false",
			SweepInterval: time.Minute,
		},
		MasterKeyRotation: coordinator.MasterKeyRotationConfig{
			RewrapConcurrency: envInt("NOVA_MASTER_KEY_REWRAP_CONCURRENCY", 4),
			RewrapBatchSize:   envInt("NOVA_MASTER_KEY_REWRAP_BATCH", 256),
			RewrapPace:        time.Duration(envInt("NOVA_MASTER_KEY_REWRAP_PACE_MS", 50)) * time.Millisecond,
		},
		ContentLifecycle: coordinator.ContentLifecycleConfig{
			SweepEnabled:    os.Getenv("NOVA_SOFT_DELETE_SWEEP_ENABLED") != "false",
			SoftDeleteGrace: time.Duration(envInt("NOVA_SOFT_DELETE_GRACE_SECONDS", 86400)) * time.Second,
			SweepInterval:   time.Duration(envInt("NOVA_LIFECYCLE_SWEEP_INTERVAL_MS", 60000)) * time.Millisecond,
		},
		AdminSPA: coordinator.AdminSPAConfig{
			DistDir: os.Getenv("NOVA_ADMIN_DIST_DIR"),
		},
		Widget: coordinator.WidgetConfig{
			DistDir: os.Getenv("NOVA_WIDGET_DIST_DIR"),
		},
		CORS: func() config.CORS {
			if opCfg != nil {
				return opCfg.Uploads.CORS
			}
			return config.CORS{}
		}(),
		UploadLimits: func() config.UploadLimits {
			if opCfg != nil {
				return opCfg.Uploads.Limits // already defaulted by applyUploadDefaults
			}
			return config.UploadLimits{
				MaxConcurrentGlobal:     config.DefaultMaxConcurrentGlobalUploads,
				MaxConcurrentPerSession: config.DefaultMaxConcurrentPerSession,
				MaxFilesPerSession:      config.DefaultMaxFilesPerSession,
			}
		}(),
		ConfigStore:    cfgStore,
		ConfigFilePath: cfgPath,
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
		"paranoid", rc.Paranoid,
		"record_source_ip", recordIP,
		"trusted_proxies", len(trustedProxies),
	)
	// Surface privacy-preset consequence warnings (paranoid on but a protective
	// default was explicitly relaxed). Empty in the default posture. P2-M0.2.
	if opCfg != nil {
		for _, w := range opCfg.PrivacyWarnings() {
			slog.Warn("privacy posture", "detail", w)
		}
	}

	// Federation control channel (P2-M2). Enabled when operator.yaml sets
	// federation.listen_addr. Bound BEFORE serving so a dead federation listener
	// fails startup rather than leaving the public coordinator silently up.
	if opCfg != nil && opCfg.Federation.Enabled() {
		fed := opCfg.Federation
		dev := isLoopback(fed.ListenAddr)
		if err := fed.Validate(dev); err != nil {
			return fmt.Errorf("federation config: %w", err)
		}
		caPEM, err := os.ReadFile(fed.FederationCAPath)
		if err != nil {
			return fmt.Errorf("federation ca: %w", err)
		}
		certPEM, err := os.ReadFile(fed.FederationCertPath)
		if err != nil {
			return fmt.Errorf("federation cert: %w", err)
		}
		keyPEM, err := os.ReadFile(fed.FederationKeyPath)
		if err != nil {
			return fmt.Errorf("federation key: %w", err)
		}
		hb, poll, conc := fed.FederationTimers()
		fedSrv := fedcoord.New(gen.New(pool), fedcoord.Config{
			ListenAddr: fed.ListenAddr,
			Timers:     wire.ConfigUpdates{HeartbeatIntervalSeconds: hb, PinsPollIntervalSeconds: poll, MaxPinConcurrency: conc},
			TLS:        fedcoord.TLSMaterial{CAPEM: caPEM, CertPEM: certPEM, KeyPEM: keyPEM},
		})
		if err := fedSrv.Listen(); err != nil {
			return fmt.Errorf("federation listen %s: %w", fed.ListenAddr, err)
		}
		slog.Info("federation listener bound", "listen", fedSrv.Addr())
		return runBoth(ctx, c.Run, fedSrv.Run)
	}
	return c.Run(ctx)
}

// runBoth runs each function concurrently under a derived context; the first
// non-nil error cancels the rest and is returned. A clean (nil) return from one
// runner does NOT cancel the others — only an error does.
func runBoth(ctx context.Context, runs ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(runs))
	for _, run := range runs {
		run := run
		go func() { errc <- run(ctx) }()
	}
	var first error
	for range runs {
		if e := <-errc; e != nil && first == nil {
			first = e
			cancel()
		}
	}
	return first
}

// isLoopback reports whether addr's host is a loopback address (dev/test mode
// where the nebula_interface guard is skipped).
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// buildAuthConfig assembles the coordinator's auth wiring from the environment.
// When NOVA_AUTH_ISSUER_URL is set the coordinator runs in external-OIDC mode
// (verify-only; local issuer endpoints 404). Otherwise it runs the built-in
// local Ed25519 issuer, which requires NOVA_OIDC_SIGNING_KEY (refuse-to-start
// floor). Public uploads require NOVA_TOS_URL (T1.20).
func buildAuthConfig(ctx context.Context, q *gen.Queries, rc resolvedConfig) (coordinator.AuthConfig, error) {
	var ac coordinator.AuthConfig

	publicUploads := rc.PublicUploads
	if publicUploads && rc.TosURL == "" {
		return ac, errors.New("NOVA_PUBLIC_UPLOADS=true requires NOVA_TOS_URL (T1.20); refusing to start")
	}
	ac.PublicUploads = publicUploads
	// Strict per-IP limiter on /api/v1/auth/login: ~5 attempts/minute, burst 5.
	ac.LoginRate = coordinator.RateLimitConfig{RatePerSec: 5.0 / 60.0, Burst: 5}

	// Scoped upload-token verifier (P2-M0.3). Chained alongside the primary
	// verifier in both modes; ErrTokenNotForMe (non-nova_ut_ tokens) makes the
	// chain order irrelevant for correctness.
	utVer := uploadtoken.New(q)

	if issuerURL := rc.AuthIssuerURL; issuerURL != "" {
		// External-OIDC mode: verify-only. New is resilient to IdP downtime
		// (background discovery retry) and only errors on invalid config.
		clientID := rc.AuthClientID
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
		ac.Verifiers = []auth.Verifier{ver, utVer}
		ac.Issuer = nil
		ac.Descriptor = api.AuthConfigDescriptor{Mode: "external", IssuerURL: issuerURL, ClientID: clientID}
		return ac, nil
	}

	// Local-issuer mode: load the Ed25519 signing key (refuse to start if absent).
	seed, signerSrc, err := secret.ResolveSecret("NOVA_OIDC_SIGNING_KEY", "NOVA_OIDC_SIGNING_KEY_FILE", "/run/secrets/oidc-signing-key")
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
	ac.Verifiers = []auth.Verifier{localIss.Verifier(), utVer}
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

// resolvedConfig holds the operator-facing values that operator.yaml may set
// and env vars may override. Deep tuning knobs (signed-url, rotation, sweeps)
// stay env-only and are not part of this struct.
type resolvedConfig struct {
	PublicUploads         bool
	TosURL                string
	Paranoid              bool
	AuthIssuerURL         string // external OIDC issuer ("" = built-in local issuer)
	AuthClientID          string
	MaxUploadSizeBytes    int64
	MaxConcurrentAssembly int
}

// resolveOperatorConfig merges operator.yaml (cfg, may be nil) with env overrides.
// getenv is injected for testability (pass os.Getenv in production).
func resolveOperatorConfig(cfg *config.Config, getenv func(string) string) resolvedConfig {
	rc := resolvedConfig{
		MaxUploadSizeBytes:    config.DefaultMaxUploadSizeBytes,
		MaxConcurrentAssembly: config.DefaultMaxConcurrentAssembly,
	}
	if cfg != nil {
		rc.PublicUploads = cfg.Uploads.PublicUploads
		rc.TosURL = cfg.TosURL
		rc.Paranoid = cfg.Auth.Paranoid
		rc.AuthIssuerURL = cfg.Auth.IssuerURL
		rc.AuthClientID = cfg.Auth.ClientID
		if cfg.Uploads.MaxUploadSizeBytes > 0 {
			rc.MaxUploadSizeBytes = cfg.Uploads.MaxUploadSizeBytes
		}
		if cfg.Uploads.MaxConcurrentAssembly > 0 {
			rc.MaxConcurrentAssembly = cfg.Uploads.MaxConcurrentAssembly
		}
	}
	// env overrides (only when the var is SET — use a presence check for bools)
	if v, ok := lookupBool(getenv, "NOVA_PUBLIC_UPLOADS"); ok {
		rc.PublicUploads = v
	}
	if v := getenv("NOVA_TOS_URL"); v != "" {
		rc.TosURL = v
	}
	if v, ok := lookupBool(getenv, "NOVA_PARANOID"); ok {
		rc.Paranoid = v
	}
	if v := getenv("NOVA_AUTH_ISSUER_URL"); v != "" {
		rc.AuthIssuerURL = v
	}
	if v := getenv("NOVA_AUTH_CLIENT_ID"); v != "" {
		rc.AuthClientID = v
	}
	if v := getenv("NOVA_MAX_UPLOAD_SIZE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rc.MaxUploadSizeBytes = n
		}
	}
	if v := getenv("NOVA_MAX_CONCURRENT_ASSEMBLY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rc.MaxConcurrentAssembly = n
		}
	}
	return rc
}

// lookupBool reports the bool value of key and whether it was set (non-empty).
func lookupBool(getenv func(string) string, key string) (val, ok bool) {
	v := getenv(key)
	if v == "" {
		return false, false
	}
	return v == "true", true
}

// operatorConfigPath returns the resolved operator.yaml path, or "" when none is configured.
func operatorConfigPath() string {
	path := os.Getenv("NOVA_CONFIG_FILE")
	if path == "" {
		if dir := os.Getenv("NOVA_CONFIG_DIR"); dir != "" {
			path = filepath.Join(dir, "operator.yaml")
		}
	}
	return path
}

// loadOperatorConfigFile loads operator.yaml from NOVA_CONFIG_FILE (or
// $NOVA_CONFIG_DIR/operator.yaml). Returns (nil, nil) when no path is
// configured or the file is absent — the env-only path (full back-compat).
func loadOperatorConfigFile() (*config.Config, error) {
	path := operatorConfigPath()
	if path == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return config.LoadFromFile(path)
}

// applyEnvOverridesTo re-applies NOVA_* overrides onto a config so env keeps
// winning over yaml on every hot reload. Mirrors resolveOperatorConfig's env
// reads. NOVA_PARANOID is re-applied below, but the downstream privacy-preset
// consequences (record_source_ip, retention, webhooks) are resolved at boot
// and are restart-effect, so they are intentionally not recomputed here.
func applyEnvOverridesTo(c *config.Config, getenv func(string) string) {
	if v, ok := lookupBool(getenv, "NOVA_PUBLIC_UPLOADS"); ok {
		c.Uploads.PublicUploads = v
	}
	if v := getenv("NOVA_TOS_URL"); v != "" {
		c.TosURL = v
	}
	if v, ok := lookupBool(getenv, "NOVA_PARANOID"); ok {
		c.Auth.Paranoid = v
	}
	if v := getenv("NOVA_AUTH_ISSUER_URL"); v != "" {
		c.Auth.IssuerURL = v
	}
	if v := getenv("NOVA_AUTH_CLIENT_ID"); v != "" {
		c.Auth.ClientID = v
	}
	if v := getenv("NOVA_MAX_UPLOAD_SIZE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.Uploads.MaxUploadSizeBytes = n
		}
	}
	if v := getenv("NOVA_MAX_CONCURRENT_ASSEMBLY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Uploads.MaxConcurrentAssembly = n
		}
	}
}

// envPinnedKeys lists the dotted config keys currently overridden by NOVA_* env,
// surfaced as source:"env" by the config admin API.
func envPinnedKeys(getenv func(string) string) map[string]struct{} {
	pins := map[string]struct{}{}
	add := func(env, key string) {
		if getenv(env) != "" {
			pins[key] = struct{}{}
		}
	}
	add("NOVA_PUBLIC_UPLOADS", "uploads.public_uploads")
	add("NOVA_TOS_URL", "tos_url")
	add("NOVA_PARANOID", "auth.paranoid")
	add("NOVA_AUTH_ISSUER_URL", "auth.issuer_url")
	add("NOVA_AUTH_CLIENT_ID", "auth.client_id")
	add("NOVA_MAX_UPLOAD_SIZE_BYTES", "uploads.max_upload_size_bytes")
	add("NOVA_MAX_CONCURRENT_ASSEMBLY", "uploads.max_concurrent_assembly")
	return pins
}
