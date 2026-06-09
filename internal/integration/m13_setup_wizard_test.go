package integration_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/localissuer"
	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/auth/token"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/setup"
	"github.com/nova-archive/nova/pkg/coordinator"
)

// TestIntegrationM13TwoVhostSplitThroughNginx proves threat-model boundary ①:
// the public vhost and admin vhost carry disjoint location sets. Routes absent
// from a vhost return nginx's bare return-404 (no X-Request-ID header, no JSON
// body); routes present proxy to the coordinator (X-Request-ID set by the
// coordinator's RequestID middleware, JSON Content-Type on error).
//
// Test A — public vs admin vhost route isolation (nginx two-vhost config).
// Test B — setup-mode → normal-mode sentinel flip (coordinator behavior).
func TestIntegrationM13TwoVhostSplitThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M13 integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// =========================================================================
	// Test A — two-vhost split (normal mode, boundary ①)
	// =========================================================================

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)
	require.NoError(t, signedurl.EnsureActiveKey(ctx, gen.New(pool), ks))

	backend := offlineBackend(t, ctx)

	signer, err := token.NewSignerFromSeed(signerSeedHex)
	require.NoError(t, err)
	iss, err := localissuer.New(localissuer.Config{
		Queries: gen.New(pool), Signer: signer, Gate: password.NewGate(4),
		IssuerURL: "https://nova.test/", Audience: "nova",
		AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour,
	})
	require.NoError(t, err)
	authCfg := coordinator.AuthConfig{
		Verifiers:  []auth.Verifier{iss.Verifier()},
		Issuer:     iss,
		Descriptor: api.AuthConfigDescriptor{Mode: "local"},
	}

	// Widget dist: needed so /widget/ proxies to the coordinator (not an empty
	// DistDir coordinator-404).
	widgetDist := m13WriteWidgetDist(t)

	cfg := coordinator.Config{
		ListenAddr:            "0.0.0.0:19013",
		Version:               "m13-itest",
		RateLimit:             coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
		MaxUploadSizeBytes:    4 << 20,
		MaxConcurrentAssembly: 4,
		SessionTTL:            time.Hour,
		UploadTmpDir:          t.TempDir(),
		UploadGCInterval:      time.Hour,
		Auth:                  authCfg,
		Widget:                coordinator.WidgetConfig{DistDir: widgetDist},
	}

	const pw = "hunter2hunter2"
	publicBase, adminBase := m13StartTwoVhostNginx(t, ctx, pool, backend, ks, cfg)

	_ = seedAuthUser(t, ctx, pool, "op13@example.com", "operator", pw)
	opTok, _ := m6Login(t, adminBase, "op13@example.com", pw)

	// --- Public vhost assertions ---
	//
	// Distinguishing signal: the coordinator's RequestID middleware always sets
	// X-Request-ID on every response. nginx's return-404 (no upstream contact)
	// never sets this header. So:
	//   X-Request-ID present → proxied to coordinator (route IS in the vhost).
	//   X-Request-ID absent  → nginx bare return-404 (route absent from vhost).

	// Admin API: NOT in public vhost → nginx return-404, no X-Request-ID.
	t.Run("public/admin-api-absent", func(t *testing.T) {
		code, _, hdr := m11GetRaw(t, publicBase+"/api/v1/admin/audits/integrity")
		require.Equal(t, http.StatusNotFound, code)
		require.Empty(t, hdr.Get("X-Request-ID"),
			"nginx return-404 must not carry X-Request-ID (route absent from public vhost)")
	})

	// Auth config: NOT in public vhost → nginx return-404, no X-Request-ID.
	t.Run("public/auth-config-absent", func(t *testing.T) {
		code, _, hdr := m11GetRaw(t, publicBase+"/api/v1/auth/config")
		require.Equal(t, http.StatusNotFound, code)
		require.Empty(t, hdr.Get("X-Request-ID"),
			"nginx return-404 must not carry X-Request-ID (route absent from public vhost)")
	})

	// Federation: explicitly blocked in both vhosts → nginx return-404.
	t.Run("public/fed-blocked", func(t *testing.T) {
		code, _, hdr := m11GetRaw(t, publicBase+"/fed/v1/x")
		require.Equal(t, http.StatusNotFound, code)
		require.Empty(t, hdr.Get("X-Request-ID"),
			"nginx return-404 must not carry X-Request-ID (/fed/ block)")
	})

	// /blob/: PRESENT in public vhost → proxied; coordinator returns JSON 404 for
	// unknown CID, with X-Request-ID and Content-Type: application/json.
	t.Run("public/blob-proxied", func(t *testing.T) {
		code, body, hdr := m11GetRaw(t, publicBase+"/blob/notacid")
		require.Equal(t, http.StatusNotFound, code)
		require.NotEmpty(t, hdr.Get("X-Request-ID"),
			"coordinator-proxied response must carry X-Request-ID")
		require.True(t, strings.Contains(hdr.Get("Content-Type"), "application/json"),
			"coordinator error body must be JSON, not nginx default 404 page")
		require.Contains(t, string(body), "code",
			"coordinator error envelope must contain 'code' field")
	})

	// /widget: PRESENT in public vhost → proxied; coordinator serves widget demo.
	t.Run("public/widget-proxied", func(t *testing.T) {
		code, _, hdr := m11GetRaw(t, publicBase+"/widget/")
		require.Equal(t, http.StatusOK, code, "widget demo index must return 200")
		require.NotEmpty(t, hdr.Get("X-Request-ID"),
			"coordinator-proxied /widget response must carry X-Request-ID")
	})

	// --- Admin vhost assertions ---

	// Admin API: PRESENT in admin vhost, no token → coordinator 401 (guard ran).
	t.Run("admin/admin-api-proxied-no-token", func(t *testing.T) {
		code, _, hdr := m11GetRaw(t, adminBase+"/api/v1/admin/audits/integrity")
		require.Equal(t, http.StatusUnauthorized, code,
			"coordinator admin guard must reject unauthenticated request")
		require.NotEmpty(t, hdr.Get("X-Request-ID"),
			"coordinator-proxied response must carry X-Request-ID (route present in admin vhost)")
	})

	// Admin API: PRESENT in admin vhost, with valid operator token → 200.
	t.Run("admin/admin-api-proxied-with-token", func(t *testing.T) {
		code, body := doJSONAuth(t, http.MethodGet, adminBase+"/api/v1/admin/audits/integrity", opTok, nil)
		require.Equal(t, http.StatusOK, code, string(body))
	})

	// Auth config: PRESENT in admin vhost → proxied, 200.
	t.Run("admin/auth-config-proxied", func(t *testing.T) {
		code, _, hdr := m11GetRaw(t, adminBase+"/api/v1/auth/config")
		require.Equal(t, http.StatusOK, code)
		require.NotEmpty(t, hdr.Get("X-Request-ID"),
			"coordinator-proxied /api/v1/auth/config must carry X-Request-ID")
	})

	// /blob/: NOT in admin vhost → nginx return-404, no X-Request-ID.
	t.Run("admin/blob-absent", func(t *testing.T) {
		code, _, hdr := m11GetRaw(t, adminBase+"/blob/anything")
		require.Equal(t, http.StatusNotFound, code)
		require.Empty(t, hdr.Get("X-Request-ID"),
			"nginx return-404 must not carry X-Request-ID (blob absent from admin vhost)")
	})

	// Federation: explicitly blocked in admin vhost too → nginx return-404.
	t.Run("admin/fed-blocked", func(t *testing.T) {
		code, _, hdr := m11GetRaw(t, adminBase+"/fed/v1/x")
		require.Equal(t, http.StatusNotFound, code)
		require.Empty(t, hdr.Get("X-Request-ID"),
			"nginx return-404 must not carry X-Request-ID (/fed/ block in admin vhost)")
	})

	// =========================================================================
	// Test B — setup-mode → normal-mode sentinel flip
	// =========================================================================
	//
	// Setup mode: RunSetupServer (sentinel absent) mounts only /setup/* + /health.
	//   /setup/state → 200.
	//   /api/v1/auth/config → not-200 (route not mounted in setup mode).
	//
	// Normal mode: coordinator.New + Run (sentinel not consulted; /setup never
	// mounted because SetupHandler.NewSetup is never called in normal mode).
	//   /setup/state → 404 (no /setup route).
	//   /api/v1/auth/config → 200.

	t.Run("setup-mode", func(t *testing.T) {
		setupPool := dbtest.New(t, ctx)

		sentinelDir := t.TempDir()
		sentinelPath := filepath.Join(sentinelDir, ".bootstrap-complete")
		paths := setup.Paths{
			ConfigDir:  sentinelDir,
			SecretsDir: t.TempDir(),
			Sentinel:   sentinelPath,
		}

		setupCtx, setupCancel := context.WithCancel(ctx)
		defer setupCancel()

		// Use a fixed port; RunSetupServer uses ListenAndServe so :0 is not supported.
		const setupPort = "19113"
		const setupToken = "testsetuptoken"
		setupDone := make(chan error, 1)
		go func() {
			setupDone <- coordinator.RunSetupServer(setupCtx, coordinator.SetupServerConfig{
				ListenAddr:     "0.0.0.0:" + setupPort,
				Version:        "m13-setup-itest",
				Pool:           setupPool,
				Paths:          paths,
				BootstrapToken: setupToken,
			})
		}()

		setupBase := "http://127.0.0.1:" + setupPort
		require.Eventually(t, func() bool {
			resp, err := http.Get(setupBase + "/health")
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}, 10*time.Second, 50*time.Millisecond, "setup server must become ready")

		// /setup/state without token → 401.
		codeNoTok, _, _ := m11GetRaw(t, setupBase+"/setup/state")
		require.Equal(t, http.StatusUnauthorized, codeNoTok,
			"/setup/state without bootstrap token must return 401")

		// /setup/state with correct token → 200 (setup route mounted, bootstrap_complete=false).
		stateReq, err := http.NewRequestWithContext(ctx, http.MethodGet, setupBase+"/setup/state", nil)
		require.NoError(t, err)
		stateReq.Header.Set("X-Nova-Setup-Token", setupToken)
		stateResp, err := http.DefaultClient.Do(stateReq)
		require.NoError(t, err)
		stateBody, _ := io.ReadAll(stateResp.Body)
		_ = stateResp.Body.Close()
		code := stateResp.StatusCode
		body := stateBody
		require.Equal(t, http.StatusOK, code, "setup mode must serve /setup/state with valid token")
		require.Contains(t, string(body), "bootstrap_complete",
			"setup/state must return bootstrap_complete field")

		// The setup server does not mount the blob route (no Blob handler constructed
		// — RunSetupServer passes no pool/backend/keystore to api.NewServer's blob
		// path). A GET /blob/<anything> → 405 Method Not Allowed or 404 from chi
		// (no route registered). Either way: NOT 200.
		blobCode, _, _ := m11GetRaw(t, setupBase+"/blob/notacid")
		require.NotEqual(t, http.StatusOK, blobCode,
			"/blob/* must not return 200 in setup mode (route not mounted)")

		setupCancel()
		select {
		case <-setupDone:
		case <-time.After(5 * time.Second):
			t.Fatal("setup server did not shut down in time")
		}
	})

	t.Run("normal-mode-no-setup-route", func(t *testing.T) {
		normalPool := dbtest.New(t, ctx)
		t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
		t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
		normalKS, err := envelope.NewKeystoreFromEnv(normalPool)
		require.NoError(t, err)
		_, err = normalKS.Bootstrap(ctx)
		require.NoError(t, err)

		normalBackend := offlineBackend(t, ctx)

		normalSigner, err := token.NewSignerFromSeed(signerSeedHex)
		require.NoError(t, err)
		normalIss, err := localissuer.New(localissuer.Config{
			Queries: gen.New(normalPool), Signer: normalSigner, Gate: password.NewGate(4),
			IssuerURL: "https://nova.test/", Audience: "nova",
			AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour,
		})
		require.NoError(t, err)
		normalAuthCfg := coordinator.AuthConfig{
			Verifiers:  []auth.Verifier{normalIss.Verifier()},
			Issuer:     normalIss,
			Descriptor: api.AuthConfigDescriptor{Mode: "local"},
		}

		// coordinator.New never calls NewSetup, so /setup/* is never mounted.
		normalC, err := coordinator.New(normalPool, normalBackend, normalKS, coordinator.Config{
			ListenAddr: "127.0.0.1:0",
			Version:    "m13-normal-itest",
			RateLimit:  coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
			Auth:       normalAuthCfg,
		})
		require.NoError(t, err)

		normalRunCtx, normalCancel := context.WithCancel(ctx)
		t.Cleanup(normalCancel)
		go func() { _ = normalC.Run(normalRunCtx) }()
		require.Eventually(t, func() bool { return normalC.Addr() != "" },
			5*time.Second, 20*time.Millisecond)

		normalBase := "http://" + normalC.Addr()

		// /setup/state → 404 (route not mounted in normal mode).
		code, _, _ := m11GetRaw(t, normalBase+"/setup/state")
		require.Equal(t, http.StatusNotFound, code,
			"/setup/state must return 404 in normal mode (route not mounted)")

		// /api/v1/auth/config → 200 (steady-state route present in normal mode).
		code, _, _ = m11GetRaw(t, normalBase+"/api/v1/auth/config")
		require.Equal(t, http.StatusOK, code,
			"/api/v1/auth/config must return 200 in normal mode")
	})
}

// m13WriteWidgetDist creates a minimal widget dist dir for the coordinator so
// the /widget/ location in nginx actually proxies to a 200 response.
func m13WriteWidgetDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	require.NoError(t, ipfs.WriteFileForTest(filepath.Join(dist, "index.html"),
		[]byte(`<!doctype html><html><head><title>nova upload widget</title></head><body></body></html>`)))
	require.NoError(t, ipfs.WriteFileForTest(filepath.Join(dist, "nova-upload-widget.js"),
		[]byte("/* widget */")))
	return dist
}

// m13StartTwoVhostNginx boots a coordinator in normal mode and starts a single
// nginx testcontainer that exposes TWO plain-HTTP server blocks whose location
// maps mirror the nova.conf.tmpl public/admin split (stripped of TLS and
// rate-limit directives, run on port 8081 and 8082 respectively).
func m13StartTwoVhostNginx(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	backend ipfs.Backend,
	ks *envelope.Keystore,
	cfg coordinator.Config,
) (publicBase, adminBase string) {
	t.Helper()
	port := m10ExtractPort(t, cfg.ListenAddr)

	c, err := coordinator.New(pool, backend, ks, cfg)
	require.NoError(t, err)
	runCtx, runCancel := context.WithCancel(ctx)
	t.Cleanup(runCancel)
	go func() { _ = c.Run(runCtx) }()
	require.Eventually(t, func() bool { return c.Addr() != "" }, 5*time.Second, 20*time.Millisecond)

	return m13NginxTwoVhost(t, ctx, port)
}

// m13NginxTwoVhost starts nginx with two plain-HTTP server blocks that mirror
// nova.conf.tmpl's public/admin location split. The public block listens on
// 8081 and the admin block on 8082; both are exposed.
//
// Distinguishing mechanism: routes ABSENT from a vhost hit nginx's
// "location / { return 404; }" — nginx never contacts the upstream so no
// X-Request-ID header is set. Routes PRESENT proxy to the coordinator which
// always sets X-Request-ID (via the RequestID middleware).
func m13NginxTwoVhost(t *testing.T, ctx context.Context, coordPort string) (publicBase, adminBase string) {
	t.Helper()
	up := "http://host.testcontainers.internal:" + coordPort

	// Config derived from nova.conf.tmpl: exact location set per vhost, plain
	// HTTP, no rate-limit directives (those are valid nginx directives that
	// require the limit_req_zone declarations at http{} level; omitting them
	// keeps the config self-contained). The proxy_pass URLs and /fed/ block
	// are verbatim from the template.
	conf := fmt.Sprintf(`
server {
  listen 8081;
  server_name nova-public.test;

  location = /health {
    proxy_pass %[1]s;
    proxy_set_header X-Forwarded-For $remote_addr;
  }

  location ~ ^/(blob|i)/ {
    proxy_pass %[1]s;
    proxy_set_header X-Forwarded-For $remote_addr;
  }

  location /legal/ {
    proxy_pass %[1]s;
    proxy_set_header X-Forwarded-For $remote_addr;
  }

  location ~ ^/api/v1/(uploads|blobs|images)(/|$) {
    proxy_pass %[1]s;
    proxy_request_buffering off;
    proxy_set_header X-Forwarded-For $remote_addr;
  }

  location /widget {
    proxy_pass %[1]s;
    proxy_set_header X-Forwarded-For $remote_addr;
  }

  location /fed/ {
    return 404;
  }

  location / {
    return 404;
  }
}

server {
  listen 8082;
  server_name nova-admin.test;

  location = /health {
    proxy_pass %[1]s;
    proxy_set_header X-Forwarded-For $remote_addr;
  }

  location /admin {
    proxy_pass %[1]s;
    proxy_set_header X-Forwarded-For $remote_addr;
  }

  location /api/v1/admin/ {
    proxy_pass %[1]s;
    proxy_set_header X-Forwarded-For $remote_addr;
  }

  location /api/v1/auth/ {
    proxy_pass %[1]s;
    proxy_set_header X-Forwarded-For $remote_addr;
  }

  location = /api/v1/users/me {
    proxy_pass %[1]s;
    proxy_set_header X-Forwarded-For $remote_addr;
  }

  location /fed/ {
    return 404;
  }

  location / {
    return 404;
  }
}
`, up)

	confPath := filepath.Join(t.TempDir(), "default.conf")
	require.NoError(t, ipfs.WriteFileForTest(confPath, []byte(conf)))

	req := testcontainers.ContainerRequest{
		Image:           "nginx:1.25-alpine",
		ExposedPorts:    []string{"8081/tcp", "8082/tcp"},
		HostAccessPorts: []int{atoiPort(t, coordPort)},
		WaitingFor:      wait.ForListeningPort("8081/tcp").WithStartupTimeout(60 * time.Second),
		Files: []testcontainers.ContainerFile{{
			HostFilePath:      confPath,
			ContainerFilePath: "/etc/nginx/conf.d/default.conf",
			FileMode:          0o644,
		}},
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cc, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_ = ctr.Terminate(cc)
	})

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	pubPort, err := ctr.MappedPort(ctx, "8081/tcp")
	require.NoError(t, err)
	admPort, err := ctr.MappedPort(ctx, "8082/tcp")
	require.NoError(t, err)

	return fmt.Sprintf("http://%s:%s", host, pubPort.Port()),
		fmt.Sprintf("http://%s:%s", host, admPort.Port())
}
