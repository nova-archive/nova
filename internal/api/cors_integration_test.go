package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/upload"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

// stubSessionStore satisfies handlers.SessionStore with no-ops for testing.
type stubSessionStore struct{}

func (s *stubSessionStore) Create(_ context.Context, _ upload.CreateParams) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (s *stubSessionStore) Get(_ context.Context, _ uuid.UUID) (*upload.Session, error) {
	return nil, nil
}
func (s *stubSessionStore) AppendChunk(_ context.Context, _ uuid.UUID, _ int64, r io.Reader) (int64, error) {
	_, _ = io.Copy(io.Discard, r)
	return 0, nil
}
func (s *stubSessionStore) Finalize(_ context.Context, _ uuid.UUID) (*storage.PutResult, error) {
	return nil, nil
}
func (s *stubSessionStore) Abort(_ context.Context, _ uuid.UUID) error { return nil }

// stubCommitter satisfies upload.Committer with a no-op.
type stubCommitter struct{}

func (s *stubCommitter) Put(_ context.Context, r io.Reader, _ int64, _ storage.PutContext) (*storage.PutResult, error) {
	_, _ = io.Copy(io.Discard, r)
	return nil, nil
}

// TestCORSPreflightRouterIntegration verifies that an OPTIONS preflight on the
// upload route reaches the CORS middleware and receives 204 (not 405/404).
// chi only dispatches Use() middleware when a route is registered for the
// request method — the explicit r.Options("/", ...) no-op handlers inside
// mountUploads are what allow this to work.
func TestCORSPreflightRouterIntegration(t *testing.T) {
	t.Parallel()

	uh := handlers.NewUploadHandler(
		&stubSessionStore{},
		&stubCommitter{},
		104857600,
		false,
		nil,
	)

	srv := api.NewServer(api.ServerConfig{
		Version:       "test",
		Upload:        uh,
		PublicUploads: true,
		CORSConfig: config.CORS{
			Enabled:        true,
			AllowedOrigins: []string{"https://widget.test"},
			AllowedMethods: []string{"POST", "HEAD", "PATCH", "DELETE"},
			AllowedHeaders: []string{"Content-Type", "Upload-Offset", "Upload-Length"},
		},
	})

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/uploads", nil)
	req.Header.Set("Origin", "https://widget.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code,
		"OPTIONS preflight must return 204 (not 405/404); body: %s", rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("Access-Control-Allow-Methods"),
		"preflight response must include Access-Control-Allow-Methods")
	require.Equal(t, "https://widget.test", rec.Header().Get("Access-Control-Allow-Origin"),
		"preflight response must echo the allowlisted origin")
}
