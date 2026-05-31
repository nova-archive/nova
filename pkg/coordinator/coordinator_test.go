package coordinator_test

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/jobs/kinds"
	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/nova-archive/nova/pkg/coordinator/product"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Stub product (implements product.Product + optional Prewarm method).
// ---------------------------------------------------------------------------

type stubProduct struct {
	name      string
	routes    func(chi.Router)
	prewarmCh chan string // if non-nil, Prewarm sends parentCID here
}

func (s *stubProduct) Name() string                { return s.name }
func (s *stubProduct) AcceptedMimeTypes() []string { return nil }
func (s *stubProduct) AnalyzeUpload(ctx context.Context, uc *product.UploadContext, r io.Reader) (product.Metadata, *storage.ScanResult, io.Reader, error) {
	return nil, &storage.ScanResult{Action: storage.ActionAllow}, nil, nil
}
func (s *stubProduct) OnCommitted(ctx context.Context, ref *storage.CommittedRef, md product.Metadata) error {
	return nil
}
func (s *stubProduct) OnDelete(ctx context.Context, tx pgx.Tx, parentCID, newState string) error {
	return nil
}
func (s *stubProduct) RegisterRoutes(r chi.Router) {
	if s.routes != nil {
		s.routes(r)
	}
}
func (s *stubProduct) Migrations() (fs.FS, string) { return embed.FS{}, "" }

// Prewarm is NOT part of the product.Product interface; it is type-asserted
// by c.prewarm dispatch in the coordinator.
func (s *stubProduct) Prewarm(ctx context.Context, parentCID string, presets []string) error {
	if s.prewarmCh != nil {
		s.prewarmCh <- parentCID
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRegisterProductRejectsReservedPrefix(t *testing.T) {
	t.Parallel()
	c, err := coordinator.New(nil, nil, nil, coordinator.Config{ListenAddr: "127.0.0.1:0", Version: "test"})
	require.NoError(t, err)

	cases := []struct {
		name    string
		route   string
		wantErr bool
	}{
		{"evil api/v1", "/api/v1/evil", true},
		{"evil blob", "/blob/x", true},
		{"evil health", "/health", true},
		{"ok image ping", "/i/ping", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubProduct{
				name: "image",
				routes: func(r chi.Router) {
					r.Post(tc.route, func(w http.ResponseWriter, req *http.Request) {})
				},
			}
			err := c.RegisterProduct(stub)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRegisterProductRoutesReachable(t *testing.T) {
	t.Parallel()
	c, err := coordinator.New(nil, nil, nil, coordinator.Config{
		ListenAddr: "127.0.0.1:0",
		Version:    "test",
		RateLimit:  coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
	})
	require.NoError(t, err)

	stub := &stubProduct{
		name: "image",
		routes: func(r chi.Router) {
			r.Get("/i/ping", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
		},
	}
	require.NoError(t, c.RegisterProduct(stub))

	srv := httptest.NewServer(c.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/i/ping")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestWorkerPoolRunsPrewarmHandler(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	c, err := coordinator.New(pool, nil, nil, coordinator.Config{
		ListenAddr: "127.0.0.1:0",
		Version:    "test",
		RateLimit:  coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
	})
	require.NoError(t, err)

	ch := make(chan string, 1)
	stub := &stubProduct{name: "image", prewarmCh: ch}
	require.NoError(t, c.RegisterProduct(stub))

	payload, _ := json.Marshal(kinds.DerivativePrewarmPayload{ParentCID: "cidXYZ", Presets: []string{"thumb"}})
	_, err = c.Queue().Enqueue(ctx, kinds.KindDerivativePrewarm, payload)
	require.NoError(t, err)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = c.Run(runCtx) }()

	select {
	case got := <-ch:
		require.Equal(t, "cidXYZ", got)
	case <-time.After(20 * time.Second):
		t.Fatal("prewarm handler not invoked")
	}
	runCancel()
}

// ---------------------------------------------------------------------------
// Original test
// ---------------------------------------------------------------------------

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
