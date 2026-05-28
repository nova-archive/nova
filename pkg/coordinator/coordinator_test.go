package coordinator_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorRunServesHealthAndShutsDown(t *testing.T) {
	t.Parallel()
	c, err := coordinator.New(nil, nil, nil, coordinator.Config{
		ListenAddr: "127.0.0.1:0",
		Version:    "test",
		RateLimit:  coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- c.Run(ctx) }()

	var addr string
	require.Eventually(t, func() bool { addr = c.Addr(); return addr != "" }, 3*time.Second, 10*time.Millisecond)

	resp, err := http.Get("http://" + addr + "/health")
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	_ = resp.Body.Close()

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
