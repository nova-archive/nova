package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/localissuer"
	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/auth/token"
	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/lifecycle"
	"github.com/nova-archive/nova/pkg/coordinator"
)

// TestIntegrationM11AdminSPAThroughNginx exercises the M11 surface end-to-end
// through nginx: the coordinator-served admin SPA (CSP + SPA fallback), the
// owner/operator blob routes + soft-delete lifecycle (soft_deleted → grace sweep
// → tombstone + crypto-shred + unpin, with blob.* audit actions), the admin
// listings, and the role boundary.
func TestIntegrationM11AdminSPAThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M11 integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

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

	dist := m11WriteDist(t)

	cfg := coordinator.Config{
		ListenAddr:            "0.0.0.0:19011",
		Version:               "m11-itest",
		RateLimit:             coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
		MaxUploadSizeBytes:    4 << 20,
		MaxConcurrentAssembly: 4,
		SessionTTL:            time.Hour,
		UploadTmpDir:          t.TempDir(),
		UploadGCInterval:      time.Hour,
		Auth:                  authCfg,
		AdminSPA:              coordinator.AdminSPAConfig{DistDir: dist},
		// Sweep driven manually below (Tick) so the test does not race wall-clock.
		ContentLifecycle: coordinator.ContentLifecycleConfig{SweepEnabled: false},
	}
	base := startCoordinatorWithNginxCfg(t, ctx, pool, backend, ks, cfg, startNginxM11)

	const pw = "hunter2hunter2"
	_ = seedAuthUser(t, ctx, pool, "op@example.com", "operator", pw)
	_ = seedAuthUser(t, ctx, pool, "mod@example.com", "moderator", pw)
	_ = seedAuthUser(t, ctx, pool, "up@example.com", "uploader", pw)
	opTok, _ := m6Login(t, base, "op@example.com", pw)
	modTok, _ := m6Login(t, base, "mod@example.com", pw)

	b1, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("delete me"), MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)
	b2, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("keep me"), MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)

	// ---- Serving: the coordinator-served SPA through nginx (CSP + SPA fallback).
	code, body, hdr := m11GetRaw(t, base+"/admin/")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), "nova admin console", "/admin/ serves index.html")
	require.Contains(t, hdr.Get("Content-Security-Policy"), "default-src 'self'", "strict CSP")
	require.NotContains(t, hdr.Get("Content-Security-Policy"), "http://", "no external origin in CSP")

	code, body, _ = m11GetRaw(t, base+"/admin/blobs")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), "nova admin console", "deep link falls back to index.html")

	code, _, ahdr := m11GetRaw(t, base+"/admin/assets/app-cafebabe.js")
	require.Equal(t, http.StatusOK, code, "hashed asset served")
	require.Contains(t, ahdr.Get("Cache-Control"), "immutable", "hashed assets are immutable-cached")

	// ---- Admin listings + authz.
	code, lbody := doJSONAuth(t, http.MethodGet, base+"/api/v1/admin/blobs", opTok, nil)
	require.Equal(t, http.StatusOK, code, string(lbody))
	require.Contains(t, string(lbody), b1.CID, "admin blob listing includes the seeded blob")

	code, _ = doJSONAuth(t, http.MethodGet, base+"/api/v1/admin/blobs", "", nil)
	require.Equal(t, http.StatusUnauthorized, code, "no token ⇒ 401")

	code, jbody := doJSONAuth(t, http.MethodGet, base+"/api/v1/admin/jobs", opTok, nil)
	require.Equal(t, http.StatusOK, code, string(jbody))
	require.Contains(t, string(jbody), "pagination", "jobs view returns a paginated envelope")

	// ---- Owner soft-delete lifecycle (Exit #2).
	code, mbody := doJSONAuth(t, http.MethodGet, base+"/api/v1/blobs/"+b1.CID, opTok, nil)
	require.Equal(t, http.StatusOK, code, string(mbody))
	require.Contains(t, string(mbody), "active")

	rc, _ := doJSONAuth(t, http.MethodGet, base+"/blob/"+b1.CID, "", nil)
	require.Equal(t, http.StatusOK, rc, "active blob reads 200")

	code, _ = doJSONAuth(t, http.MethodDelete, base+"/api/v1/blobs/"+b1.CID, opTok, nil)
	require.Equal(t, http.StatusNoContent, code, "operator soft-deletes")

	rc, _ = doJSONAuth(t, http.MethodGet, base+"/blob/"+b1.CID, "", nil)
	require.Equal(t, http.StatusGone, rc, "soft-deleted reads return 410")
	require.Equal(t, 1, m11AuditCount(t, ctx, pool, "blob.soft_deleted", b1.CID))

	// Drive the lifecycle sweep deterministically (tiny grace ⇒ immediately overdue).
	lifeSvc := lifecycle.NewService(gen.New(pool), pool, backend, nil,
		auditlog.NewWriter(gen.New(pool), slog.Default()), slog.Default(), time.Now, time.Nanosecond)
	lifecycle.NewSweeper(lifeSvc, time.Minute, true, slog.Default()).Tick(ctx)

	rc, _ = doJSONAuth(t, http.MethodGet, base+"/blob/"+b1.CID, "", nil)
	require.Equal(t, http.StatusGone, rc, "tombstoned reads return 410")
	require.Equal(t, "shredded", m9DEKState(t, ctx, pool, b1.CID), "the sweep crypto-shreds the DEK")
	require.Equal(t, "tombstoned", m11BlobState(t, ctx, pool, b1.CID))
	require.Equal(t, 1, m11AuditCount(t, ctx, pool, "blob.tombstoned", b1.CID),
		"system blob.tombstoned audit (not a moderation dmca.* action)")
	require.Equal(t, 0, m11AuditCount(t, ctx, pool, "dmca.tombstoned", b1.CID))
	has, _ := backend.Has(ctx, b1.ParsedCID)
	require.False(t, has, "the sweep unpins the CID")

	// ---- Role boundary (Exit #5).
	code, _ = doJSONAuth(t, http.MethodGet, base+"/api/v1/blobs/"+b2.CID, modTok, nil)
	require.Equal(t, http.StatusOK, code, "moderator may read blob metadata")
	code, _ = doJSONAuth(t, http.MethodDelete, base+"/api/v1/blobs/"+b2.CID, modTok, nil)
	require.Equal(t, http.StatusForbidden, code, "moderator may not soft-delete")
	code, _ = doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/keys/rotate-master", modTok,
		map[string]any{"from_version": "v1", "to_version": "v1"})
	require.Equal(t, http.StatusForbidden, code, "rotate-master is operator-only")
	require.Equal(t, "active", m11BlobState(t, ctx, pool, b2.CID), "b2 untouched by the forbidden actions")
}

// --- M11 helpers ------------------------------------------------------------

func m11WriteDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dist, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "index.html"),
		[]byte(`<!doctype html><html><head><title>nova admin console</title></head>`+
			`<body><div id="root"></div><script type="module" src="/admin/assets/app-cafebabe.js"></script></body></html>`),
		0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "assets", "app-cafebabe.js"),
		[]byte("console.log('nova')"), 0o644))
	return dist
}

func m11GetRaw(t *testing.T, url string) (int, []byte, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func m11BlobState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) string {
	t.Helper()
	var s string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM blobs WHERE cid=$1`, cidStr).Scan(&s))
	return s
}

func m11AuditCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, action, cidStr string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action=$1 AND target_type='cid' AND target_id=$2`,
		action, cidStr).Scan(&n))
	return n
}

// startNginxM11 fronts the coordinator with the M11 proxy surface: the M10 API
// routes plus the owner blob routes (/api/v1/blobs/) and the coordinator-served
// admin SPA static prefix (/admin, distinct from /api/v1/admin).
func startNginxM11(t *testing.T, ctx context.Context, coordPort string) string {
	t.Helper()
	up := "http://host.testcontainers.internal:" + coordPort
	conf := fmt.Sprintf(`
server {
  listen 8080;
  location = /health          { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /blob/             { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /admin             { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/auth/      { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location = /api/v1/users/me { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/blobs/     { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/admin/     { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
}
`, up, up, up, up, up, up, up)

	confPath := filepath.Join(t.TempDir(), "default.conf")
	require.NoError(t, ipfs.WriteFileForTest(confPath, []byte(conf)))

	req := testcontainers.ContainerRequest{
		Image:           "nginx:1.25-alpine",
		ExposedPorts:    []string{"8080/tcp"},
		HostAccessPorts: []int{atoiPort(t, coordPort)},
		WaitingFor:      wait.ForListeningPort("8080/tcp").WithStartupTimeout(60 * time.Second),
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
	mapped, err := ctr.MappedPort(ctx, "8080/tcp")
	require.NoError(t, err)
	return fmt.Sprintf("http://%s:%s", host, mapped.Port())
}
