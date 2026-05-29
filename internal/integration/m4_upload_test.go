package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestIntegrationM4UploadThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M4 integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	swarm := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarm,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))
	backend, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath: t.TempDir(), Mode: ipfs.ModePrivate, SwarmKeyPath: swarm, Online: false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = backend.Close(c)
	})

	const coordPort = "19004"
	c, err := coordinator.New(pool, backend, ks, coordinator.Config{
		ListenAddr:            "0.0.0.0:" + coordPort,
		Version:               "m4-itest",
		RateLimit:             coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
		MaxUploadSizeBytes:    1 << 20,
		MaxConcurrentAssembly: 4,
		SessionTTL:            time.Hour,
		UploadTmpDir:          t.TempDir(),
		UploadGCInterval:      time.Hour,
	})
	require.NoError(t, err)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = c.Run(runCtx) }()
	require.Eventually(t, func() bool { return c.Addr() != "" }, 5*time.Second, 20*time.Millisecond)

	base := startNginxM4(t, ctx, coordPort)
	col := seedPublicCollection(t, ctx, pool)

	fixtures := map[string][]byte{
		"image/jpeg": {0xff, 0xd8, 0xff, 0xe0, 'J', 'F', 'I', 'F', 1, 2, 3, 4},
		"image/png":  {0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 9, 8, 7, 6, 5},
		"image/webp": append(append([]byte("RIFF"), 0, 0, 0, 0), []byte("WEBPVP8 hello!")...),
	}

	for mime, body := range fixtures {
		// tus (two chunks for jpeg to exercise resumption), then read back.
		cid := tusUpload(t, base, mime, col, body, mime == "image/jpeg")
		assertFetch(t, base, cid, body, mime)

		// multipart fallback, then read back.
		cid2 := multipartUpload(t, base, mime, col, body)
		assertFetch(t, base, cid2, body, mime)
	}

	// Negative: oversize create → 413.
	{
		req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/uploads", nil)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Length", strconv.Itoa(2<<20))
		req.Header.Set("Upload-Metadata", "mime_type "+b64("text/plain"))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
		_ = resp.Body.Close()
	}

	// Negative: declared image/jpeg but the bytes are a script → 400 at finalize.
	{
		script := []byte("#!/bin/sh\necho hello world\n")
		loc := tusCreate(t, base, "image/jpeg", &col, len(script))
		tusPatch(t, base, loc, 0, script, http.StatusNoContent)
		freq, _ := http.NewRequest(http.MethodPost, base+loc+"/finalize", nil)
		fresp, err := http.DefaultClient.Do(freq)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, fresp.StatusCode)
		_ = fresp.Body.Close()
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func seedPublicCollection(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var owner, col uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		uuid.NewString()+"@m4.test").Scan(&owner))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
		 VALUES ($1,'pub','pub','public',false) RETURNING id`, owner).Scan(&col))
	return col
}

func tusCreate(t *testing.T, base, mime string, col *uuid.UUID, length int) string {
	t.Helper()
	meta := "mime_type " + b64(mime)
	if col != nil {
		meta += ",collection_id " + b64(col.String())
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(length))
	req.Header.Set("Upload-Metadata", meta)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	loc := resp.Header.Get("Location")
	_ = resp.Body.Close()
	require.NotEmpty(t, loc)
	return loc
}

func tusPatch(t *testing.T, base, loc string, offset int, chunk []byte, wantStatus int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, base+loc, bytes.NewReader(chunk))
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Offset", strconv.Itoa(offset))
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, wantStatus, resp.StatusCode)
	_ = resp.Body.Close()
}

func tusUpload(t *testing.T, base, mime string, col uuid.UUID, body []byte, twoChunks bool) string {
	t.Helper()
	loc := tusCreate(t, base, mime, &col, len(body))
	if twoChunks && len(body) > 1 {
		mid := len(body) / 2
		tusPatch(t, base, loc, 0, body[:mid], http.StatusNoContent)
		tusPatch(t, base, loc, mid, body[mid:], http.StatusNoContent)
	} else {
		tusPatch(t, base, loc, 0, body, http.StatusNoContent)
	}
	freq, _ := http.NewRequest(http.MethodPost, base+loc+"/finalize", nil)
	fresp, err := http.DefaultClient.Do(freq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, fresp.StatusCode)
	cid := decodeCID(t, fresp.Body)
	_ = fresp.Body.Close()
	return cid
}

func multipartUpload(t *testing.T, base, mime string, col uuid.UUID, body []byte) string {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="f"`)
	hdr.Set("Content-Type", mime)
	part, err := w.CreatePart(hdr)
	require.NoError(t, err)
	_, _ = part.Write(body)
	require.NoError(t, w.WriteField("collection_id", col.String()))
	require.NoError(t, w.Close())

	resp, err := http.Post(base+"/api/v1/blobs", w.FormDataContentType(), &b)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	cid := decodeCID(t, resp.Body)
	_ = resp.Body.Close()
	return cid
}

func decodeCID(t *testing.T, r io.Reader) string {
	t.Helper()
	var out struct {
		CID string `json:"cid"`
	}
	require.NoError(t, json.NewDecoder(r).Decode(&out))
	require.NotEmpty(t, out.CID)
	return out.CID
}

func assertFetch(t *testing.T, base, cid string, want []byte, mime string) {
	t.Helper()
	resp, err := http.Get(base + "/blob/" + cid)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, mime, resp.Header.Get("Content-Type"))
}

// startNginxM4 launches an nginx container proxying /health, /blob/, and the
// M4 write routes to the in-process coordinator. Reuses atoiPort from the M3
// test (same package).
func startNginxM4(t *testing.T, ctx context.Context, coordPort string) string {
	t.Helper()
	up := "http://host.testcontainers.internal:" + coordPort
	conf := fmt.Sprintf(`
server {
  listen 8080;
  location = /health        { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /blob/           { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/uploads  { proxy_pass %s; proxy_request_buffering off; proxy_set_header X-Forwarded-For $remote_addr; }
  location = /api/v1/blobs  { proxy_pass %s; proxy_request_buffering off; proxy_set_header X-Forwarded-For $remote_addr; }
}
`, up, up, up, up)

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
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = ctr.Terminate(c)
	})

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	mapped, err := ctr.MappedPort(ctx, "8080/tcp")
	require.NoError(t, err)
	return fmt.Sprintf("http://%s:%s", host, mapped.Port())
}
