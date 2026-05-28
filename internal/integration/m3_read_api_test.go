package integration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestIntegrationM3ReadThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M3 integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
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

	const coordPort = "19000"
	c, err := coordinator.New(pool, backend, ks, coordinator.Config{
		ListenAddr: "0.0.0.0:" + coordPort,
		Version:    "m3-itest",
		RateLimit:  coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
	})
	require.NoError(t, err)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = c.Run(runCtx) }()
	require.Eventually(t, func() bool { return c.Addr() != "" }, 5*time.Second, 20*time.Millisecond)

	// Main blob: ~3 MiB random, exercises the dag-pb import + whole-object
	// decrypt memory budget (documented Phase-1 tested size).
	plaintext := make([]byte, 3*1024*1024)
	_, err = rand.Read(plaintext)
	require.NoError(t, err)
	res, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: plaintext, MIME: "application/octet-stream", Visibility: "public"})
	require.NoError(t, err)

	base := startNginx(t, ctx, coordPort)

	hresp, err := http.Get(base + "/health")
	require.NoError(t, err)
	require.Equal(t, 200, hresp.StatusCode)
	_ = hresp.Body.Close()

	bresp, err := http.Get(base + "/blob/" + res.CID)
	require.NoError(t, err)
	defer bresp.Body.Close()
	require.Equal(t, 200, bresp.StatusCode)
	got, err := io.ReadAll(bresp.Body)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
	require.Equal(t, "application/octet-stream", bresp.Header.Get("Content-Type"))
	require.Equal(t, `"`+res.CID+`"`, bresp.Header.Get("ETag"))

	requireStatus(t, base+"/blob/not-a-cid", 404)

	req, _ := http.NewRequest("GET", base+"/blob/"+res.CID, nil)
	req.Header.Set("Range", "bytes=0-3")
	rr, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rr.StatusCode)
	_ = rr.Body.Close()

	pv, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("secret"), MIME: "text/plain", Visibility: "private"})
	require.NoError(t, err)
	requireStatus(t, base+"/blob/"+pv.CID, 401)
	requireStatus(t, base+"/blob/"+pv.CID+".json", 404)
	requireStatus(t, base+"/blob/"+res.CID+".json", 200)
}

func requireStatus(t *testing.T, url string, want int) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, want, resp.StatusCode, "GET %s", url)
}

// startNginx launches an nginx container whose config proxies to the
// in-process coordinator on the host. Returns the base URL.
func startNginx(t *testing.T, ctx context.Context, coordPort string) string {
	t.Helper()
	conf := fmt.Sprintf(`
server {
  listen 8080;
  location = /health { proxy_pass http://host.testcontainers.internal:%s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /blob/    { proxy_pass http://host.testcontainers.internal:%s; proxy_set_header X-Forwarded-For $remote_addr; }
}
`, coordPort, coordPort)

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

func atoiPort(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		require.True(t, r >= '0' && r <= '9')
		n = n*10 + int(r-'0')
	}
	return n
}
