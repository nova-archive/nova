package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/nova-archive/nova/internal/setup"
	"github.com/stretchr/testify/require"
)

// do performs a single HTTP round-trip against handler r and returns the
// status code. When body is non-empty the request carries Content-Type:
// application/json. The second return value is the response body string.
func do(t *testing.T, r http.Handler, method, path, body string) (int, string) {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

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

	for _, p := range []string{"/i/bafyX", "/fed/v1/x", "/v/x", "/a/x", "/d/x", "/r/x"} {
		code, _ := do(t, srv, http.MethodGet, p, "")
		require.Equal(t, http.StatusNotFound, code, "reserved prefix %s must 404", p)
	}
}

func TestServerAuthRouting(t *testing.T) {
	t.Parallel()
	cfg := api.ServerConfig{Version: "test", AuthConfig: api.AuthConfigDescriptor{Mode: "local"}}
	r := api.NewServer(cfg)

	code, _ := do(t, r, http.MethodGet, "/api/v1/auth/config", "")
	require.Equal(t, 200, code)

	code, _ = do(t, r, http.MethodGet, "/api/v1/admin/anything", "")
	require.Equal(t, 401, code) // guard runs, no identity

	code, _ = do(t, r, http.MethodGet, "/api/v1/users/me", "")
	require.Equal(t, 401, code) // no token
}

func TestServerSetupSeam(t *testing.T) {
	t.Parallel()

	// Build a SetupHandler with a sentinel that does NOT exist ⇒ handler is non-nil.
	h := handlers.NewSetup(handlers.SetupConfig{
		Paths: setup.Paths{
			ConfigDir:  t.TempDir(),
			SecretsDir: t.TempDir(),
			Sentinel:   filepath.Join(t.TempDir(), ".bootstrap-complete"),
		},
	})
	require.NotNil(t, h, "sentinel absent ⇒ NewSetup must return non-nil")

	// With Setup mounted, GET /setup/state must return 200.
	r := api.NewServer(api.ServerConfig{Version: "test", Setup: h})
	code, _ := do(t, r, http.MethodGet, "/setup/state", "")
	require.Equal(t, http.StatusOK, code, "GET /setup/state with Setup mounted must return 200")

	// Without Setup mounted (nil), GET /setup/state must 404.
	r2 := api.NewServer(api.ServerConfig{Version: "test"})
	code2, _ := do(t, r2, http.MethodGet, "/setup/state", "")
	require.Equal(t, http.StatusNotFound, code2, "GET /setup/state without Setup mounted must 404")
}

func TestServerExternalModeAuth404(t *testing.T) {
	t.Parallel()
	cfg := api.ServerConfig{Version: "test", AuthConfig: api.AuthConfigDescriptor{Mode: "external", IssuerURL: "https://idp/"}}
	r := api.NewServer(cfg) // Issuer nil => external mode
	code, _ := do(t, r, http.MethodPost, "/api/v1/auth/login", "{}")
	require.Equal(t, 404, code)

	_, body := do(t, r, http.MethodPost, "/api/v1/auth/login", "{}")
	require.Contains(t, body, "external_oidc_active")

	code, _ = do(t, r, http.MethodGet, "/api/v1/auth/config", "")
	require.Equal(t, 200, code)
}
