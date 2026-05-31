package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
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
	novaimage "github.com/nova-archive/nova/nova-image"
	"github.com/nova-archive/nova/nova-image/imageproduct"
	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/image/webp"
)

func TestIntegrationM5ImageTransformsThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M5 integration test in short mode")
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
		cc, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_ = backend.Close(cc)
	})

	const coordPort = "19005"
	c, err := coordinator.New(pool, backend, ks, coordinator.Config{
		ListenAddr:            "0.0.0.0:" + coordPort,
		Version:               "m5-itest",
		RateLimit:             coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
		MaxUploadSizeBytes:    4 << 20,
		MaxConcurrentAssembly: 4,
		SessionTTL:            time.Hour,
		UploadTmpDir:          t.TempDir(),
		UploadGCInterval:      time.Hour,
		Auth:                  coordinator.AuthConfig{PublicUploads: true},
	})
	require.NoError(t, err)

	// Register the image product exactly as cmd/coordinator does.
	imgCfg := novaimage.DefaultConfig()
	require.NoError(t, imageproduct.Startup(imgCfg.VipsCacheMaxMemBytes))
	img := imageproduct.New(imgCfg, c.Storage(), pool, c.Queue())
	require.NoError(t, c.RegisterProduct(img))

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = c.Run(runCtx) }()
	require.Eventually(t, func() bool { return c.Addr() != "" }, 5*time.Second, 20*time.Millisecond)

	base := startNginxM5(t, ctx, coordPort)
	col := seedPublicCollection(t, ctx, pool)

	jpegBody := makeJPEG(t, 600, 400)

	// Upload via /api/v1/images (multipart, product forced to "image").
	cid, presets := imageUpload(t, base, jpegBody, &col)
	require.NotEmpty(t, cid)
	require.Equal(t, "/i/"+cid+"/p/thumb.webp", presets["thumb"])

	// w512.webp transform through nginx → 200 + a real 512-px WebP.
	{
		resp, err := http.Get(base + "/i/" + cid + "/w512.webp")
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "image/webp", resp.Header.Get("Content-Type"))
		dc, derr := webp.DecodeConfig(bytes.NewReader(body))
		require.NoError(t, derr, "derivative must be a decodable webp")
		require.Equal(t, 512, dc.Width, "w512 derivative width")
	}

	// Preset thumb.webp → 200.
	requireGetStatus(t, base+"/i/"+cid+"/p/thumb.webp", http.StatusOK)

	// Second w512 request: find-or-create keeps exactly one derivative row.
	requireGetStatus(t, base+"/i/"+cid+"/w512.webp", http.StatusOK)
	require.Equal(t, 1, countDerivatives(t, ctx, pool, cid, "w512", "webp"))

	// Prewarm: the worker pool generates thumb (webp) + og (jpeg).
	require.Eventually(t, func() bool {
		return countDerivatives(t, ctx, pool, cid, "thumb", "webp") == 1 &&
			countDerivatives(t, ctx, pool, cid, "og", "jpeg") == 1
	}, 60*time.Second, 250*time.Millisecond, "prewarm presets thumb+og should be generated")

	// tus upload of an image (product=image via metadata), then transform.
	cid2 := tusImageUpload(t, base, jpegBody, col)
	requireGetStatus(t, base+"/i/"+cid2+"/w512.webp", http.StatusOK)

	// Negatives.
	requireImageUploadStatus(t, base, []byte("definitely not an image"), "text/plain", &col, http.StatusUnsupportedMediaType) // 415
	requireGetStatus(t, base+"/i/"+cid+"/w999.webp", http.StatusBadRequest)                                                   // 400 dimension not whitelisted
	requireGetStatus(t, base+"/i/"+cid+"/w512.avif", http.StatusNotAcceptable)                                                // 406 format off by default

	// 401: a private image (no collection) requires auth on read.
	privCID, _ := imageUpload(t, base, jpegBody, nil)
	requireGetStatus(t, base+"/i/"+privCID+"/w512.webp", http.StatusUnauthorized)
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, im, &jpeg.Options{Quality: 90}))
	return b.Bytes()
}

func imageMultipart(t *testing.T, body []byte, ctype string, col *uuid.UUID) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="f"`)
	hdr.Set("Content-Type", ctype)
	part, err := w.CreatePart(hdr)
	require.NoError(t, err)
	_, _ = part.Write(body)
	if col != nil {
		require.NoError(t, w.WriteField("collection_id", col.String()))
	}
	require.NoError(t, w.Close())
	return &b, w.FormDataContentType()
}

func imageUpload(t *testing.T, base string, body []byte, col *uuid.UUID) (string, map[string]string) {
	t.Helper()
	b, ctype := imageMultipart(t, body, "image/jpeg", col)
	resp, err := http.Post(base+"/api/v1/images", ctype, b)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var out struct {
		CID  string `json:"cid"`
		URLs struct {
			Presets map[string]string `json:"presets"`
		} `json:"urls"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotEmpty(t, out.CID)
	return out.CID, out.URLs.Presets
}

func requireImageUploadStatus(t *testing.T, base string, body []byte, ctype string, col *uuid.UUID, want int) {
	t.Helper()
	b, ct := imageMultipart(t, body, ctype, col)
	resp, err := http.Post(base+"/api/v1/images", ct, b)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, want, resp.StatusCode)
}

func tusImageUpload(t *testing.T, base string, body []byte, col uuid.UUID) string {
	t.Helper()
	meta := "mime_type " + b64("image/jpeg") + ",product " + b64("image") + ",collection_id " + b64(col.String())
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(len(body)))
	req.Header.Set("Upload-Metadata", meta)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	loc := resp.Header.Get("Location")
	_ = resp.Body.Close()
	require.NotEmpty(t, loc)
	tusPatch(t, base, loc, 0, body, http.StatusNoContent)
	freq, _ := http.NewRequest(http.MethodPost, base+loc+"/finalize", nil)
	fresp, err := http.DefaultClient.Do(freq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, fresp.StatusCode)
	cid := decodeCID(t, fresp.Body)
	_ = fresp.Body.Close()
	return cid
}

func countDerivatives(t *testing.T, ctx context.Context, pool *pgxpool.Pool, parent, preset, format string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blobs WHERE parent_cid=$1 AND derivative_preset=$2 AND derivative_format=$3`,
		parent, preset, format).Scan(&n))
	return n
}

func requireGetStatus(t *testing.T, url string, want int) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, want, resp.StatusCode, "GET %s", url)
}

func startNginxM5(t *testing.T, ctx context.Context, coordPort string) string {
	t.Helper()
	up := "http://host.testcontainers.internal:" + coordPort
	conf := fmt.Sprintf(`
server {
  listen 8080;
  location = /health        { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /blob/           { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /i/              { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/uploads  { proxy_pass %s; proxy_request_buffering off; proxy_set_header X-Forwarded-For $remote_addr; }
  location = /api/v1/blobs  { proxy_pass %s; proxy_request_buffering off; proxy_set_header X-Forwarded-For $remote_addr; }
  location = /api/v1/images { proxy_pass %s; proxy_request_buffering off; proxy_set_header X-Forwarded-For $remote_addr; }
}
`, up, up, up, up, up, up)

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
