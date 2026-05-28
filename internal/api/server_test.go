package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/stretchr/testify/require"
)

func TestServerRoutesAndReservedNamespaces(t *testing.T) {
	t.Parallel()
	srv := api.NewServer(api.ServerConfig{
		Version: "test",
		Limiter: ratelimit.NewLimiter(ratelimit.Config{RatePerSec: 1000, Burst: 1000}, nil),
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	require.Equal(t, 200, rec.Code)
	require.NotEmpty(t, rec.Header().Get("X-Request-ID"))

	for _, p := range []string{"/api/v1/anything", "/i/bafyX", "/fed/v1/x", "/v/x", "/a/x", "/d/x", "/r/x"} {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, httptest.NewRequest("GET", p, nil))
		require.Equal(t, http.StatusNotFound, rr.Code, "reserved prefix %s must 404 in M3", p)
	}
}
