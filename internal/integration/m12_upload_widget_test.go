package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
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
	"github.com/nova-archive/nova/pkg/coordinator"
)

// TestIntegrationM12UploadWidgetThroughNginx exercises the M12 surface end-to-end
// through nginx: the coordinator-served widget static seam (/widget/*, strict CSP,
// caching, 404 with no SPA fallback) and the real upload lifecycle the widget
// drives (tus create → PATCH → finalize → CID → /blob render), plus the no-token
// 401 boundary with the public-uploads floor off.
func TestIntegrationM12UploadWidgetThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M12 integration test in short mode")
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
		Verifiers:     []auth.Verifier{iss.Verifier()},
		Issuer:        iss,
		Descriptor:    api.AuthConfigDescriptor{Mode: "local"},
		PublicUploads: false, // bearer required ⇒ exercises the widget's getToken path + the 401 boundary
	}

	dist := m12WriteDist(t)
	col := m12SeedPublicCollection(t, ctx, pool)

	cfg := coordinator.Config{
		ListenAddr:            "0.0.0.0:19012",
		Version:               "m12-itest",
		RateLimit:             coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
		MaxUploadSizeBytes:    4 << 20,
		MaxConcurrentAssembly: 4,
		SessionTTL:            time.Hour,
		UploadTmpDir:          t.TempDir(),
		UploadGCInterval:      time.Hour,
		Auth:                  authCfg,
		Widget:                coordinator.WidgetConfig{DistDir: dist},
	}
	base := startCoordinatorWithNginxCfg(t, ctx, pool, backend, ks, cfg, startNginxM12)

	const pw = "hunter2hunter2"
	_ = seedAuthUser(t, ctx, pool, "up@example.com", "uploader", pw)
	upTok, _ := m6Login(t, base, "up@example.com", pw)

	// ---- Serving (Exit #1): the coordinator-served widget seam through nginx.
	code, body, hdr := m11GetRaw(t, base+"/widget/")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), "nova upload widget", "/widget/ serves the demo index.html")
	require.Contains(t, hdr.Get("Content-Security-Policy"), "default-src 'self'", "strict CSP")
	require.Equal(t, "no-store", hdr.Get("Cache-Control"), "demo index is no-store")

	code, _, jhdr := m11GetRaw(t, base+"/widget/nova-upload-widget.js")
	require.Equal(t, http.StatusOK, code, "entry bundle served")
	require.Equal(t, "no-cache", jhdr.Get("Cache-Control"), "stable entry JS is no-cache")

	code, _, _ = m11GetRaw(t, base+"/widget/does-not-exist.js")
	require.Equal(t, http.StatusNotFound, code, "unknown widget path 404s (no SPA fallback)")

	// ---- Upload lifecycle (Exit #2): the tus→finalize flow the widget performs.
	// The upload is placed in a public collection so the blob is publicly
	// readable at /blob/<cid> without a signed URL (PublicUploads is off; the
	// collection visibility is what grants read access to anonymous callers).
	payload := []byte("hello from the nova upload widget")
	cid := m12TusUpload(t, base, payload, upTok, col)
	require.NotEmpty(t, cid)
	rc, rbody, _ := m11GetRaw(t, base+"/blob/"+cid)
	require.Equal(t, http.StatusOK, rc, "finalized blob reads 200")
	require.Equal(t, payload, rbody, "served bytes match the upload")

	// Resume: split into two chunks, HEAD-probe after the first half, then finalize.
	cid2 := m12TusResumeUpload(t, base, payload, upTok, col)
	require.NotEmpty(t, cid2)
	rc2, rbody2, _ := m11GetRaw(t, base+"/blob/"+cid2)
	require.Equal(t, http.StatusOK, rc2, "resumed blob reads 200")
	require.Equal(t, payload, rbody2, "resumed upload bytes match the upload")

	// ---- Token/floor (Exit #3): no token ⇒ 401 (public-uploads floor off).
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "no token ⇒ 401 with the floor off")
}

// --- M12 helpers ------------------------------------------------------------

func m12WriteDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dist, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "index.html"),
		[]byte(`<!doctype html><html><head><title>nova upload widget</title></head>`+
			`<body><div data-nova-upload-widget></div><script src="/widget/nova-upload-widget.js"></script></body></html>`),
		0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "nova-upload-widget.js"),
		[]byte("/* iife bundle */"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "assets", "uppy-cafebabe.css"),
		[]byte(".uppy{}"), 0o644))
	return dist
}

// m12TusUpload runs the full authenticated tus lifecycle the widget drives:
// create → single PATCH → finalize, returning the resulting CID.
// col is placed in the Upload-Metadata so the resulting blob is visible in the
// public collection (allowing anonymous /blob/<cid> reads even with PublicUploads
// off; visibility is granted by collection membership, not the upload floor).
func m12TusUpload(t *testing.T, base string, body []byte, tokenStr string, col uuid.UUID) string {
	t.Helper()
	loc := m12TusCreate(t, base, len(body), tokenStr, col)
	m12TusPatch(t, base, loc, 0, body, tokenStr, http.StatusNoContent)
	return m12TusFinalize(t, base, loc, tokenStr)
}

// m12TusResumeUpload proves resumable upload: PATCH the first half, HEAD-probe the
// server offset (which must equal the first chunk), then PATCH the remainder from
// that offset and finalize. Returns the resulting CID.
func m12TusResumeUpload(t *testing.T, base string, body []byte, tokenStr string, col uuid.UUID) string {
	t.Helper()
	loc := m12TusCreate(t, base, len(body), tokenStr, col)
	half := len(body) / 2
	m12TusPatch(t, base, loc, 0, body[:half], tokenStr, http.StatusNoContent)
	off := m12TusHeadOffset(t, base, loc, tokenStr)
	require.Equal(t, half, off, "server reports the resumable offset after the first chunk")
	m12TusPatch(t, base, loc, off, body[off:], tokenStr, http.StatusNoContent)
	return m12TusFinalize(t, base, loc, tokenStr)
}

func m12TusCreate(t *testing.T, base string, length int, tokenStr string, col uuid.UUID) string {
	t.Helper()
	meta := "mime_type " + b64("text/plain") + ",filename " + b64("hello.txt") +
		",product " + b64("raw") + ",collection_id " + b64(col.String())
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(length))
	req.Header.Set("Upload-Metadata", meta)
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	loc := resp.Header.Get("Location")
	_ = resp.Body.Close()
	require.NotEmpty(t, loc)
	return loc
}

// m12SeedPublicCollection creates a public collection so uploads placed in it
// are readable at /blob/<cid> without a signed URL.
func m12SeedPublicCollection(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var owner, col uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		uuid.NewString()+"@m12.test").Scan(&owner))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
		 VALUES ($1,'pub','pub-m12','public',false) RETURNING id`, owner).Scan(&col))
	return col
}

func m12TusPatch(t *testing.T, base, loc string, offset int, chunk []byte, tokenStr string, want int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, base+loc, bytes.NewReader(chunk))
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Offset", strconv.Itoa(offset))
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, want, resp.StatusCode)
}

func m12TusHeadOffset(t *testing.T, base, loc, tokenStr string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodHead, base+loc, nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	off, err := strconv.Atoi(resp.Header.Get("Upload-Offset"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	return off
}

func m12TusFinalize(t *testing.T, base, loc, tokenStr string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+loc+"/finalize", nil)
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cid := decodeCID(t, resp.Body)
	_ = resp.Body.Close()
	return cid
}

// startNginxM12 fronts the coordinator with the M12 proxy surface: the upload
// endpoints, blob reads, auth, and the coordinator-served widget static prefix
// (/widget, distinct from /api/v1/uploads).
func startNginxM12(t *testing.T, ctx context.Context, coordPort string) string {
	t.Helper()
	up := "http://host.testcontainers.internal:" + coordPort
	conf := fmt.Sprintf(`
server {
  listen 8080;
  client_max_body_size 100m;
  location = /health      { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /blob/         { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /widget        { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/auth/  { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/uploads { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; proxy_request_buffering off; }
}
`, up, up, up, up, up)

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
