package imageapi

// Unit tests for the staging-parent guard (Task 13, P2-M4.1).
//
// These tests use a lightweight fakeStore (no DB, no libvips) so they run
// without any external dependencies. The three tests cover:
//
//  1. TestPublicTransformOfStagingParentRefused — transform route returns 404
//     and never calls PutDerivative when Resolve returns ErrStagingNotVisible.
//
//  2. TestTransformOverPrunedParentRefetches — when Resolve succeeds and
//     OpenBytes returns valid image bytes (simulating a pruned-but-re-fetched
//     parent via Task-7's donor tier), the transform succeeds with 200.
//
//  3. TestPrewarmRunsOnlyAfterCommit — Prewarm aborts with an error and never
//     calls PutDerivative when Resolve returns ErrStagingNotVisible. (The
//     structural guarantee that prewarm is only *enqueued* post-commit lives in
//     Task 11's OnCommitted relocation; this test covers the behavioral guard.)

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	novaimage "github.com/nova-archive/nova/nova-image"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// fakeStore — implements Store without a database.
// ---------------------------------------------------------------------------

type fakeStore struct {
	resolveResult  *storage.BlobView
	resolveErr     error
	openBytesBytes []byte
	openBytesErr   error
	putDerivCalls  int
	getDerivResult string
	getDerivFound  bool
}

func (f *fakeStore) Resolve(_ context.Context, _ string) (*storage.BlobView, error) {
	return f.resolveResult, f.resolveErr
}

func (f *fakeStore) OpenBytes(_ context.Context, _ *storage.BlobView) (io.ReadCloser, error) {
	if f.openBytesErr != nil {
		return nil, f.openBytesErr
	}
	return io.NopCloser(bytes.NewReader(f.openBytesBytes)), nil
}

func (f *fakeStore) PutDerivative(_ context.Context, data []byte, dc storage.DerivativeContext, persist func(context.Context, pgx.Tx, string) error) (*storage.PutResult, error) {
	f.putDerivCalls++
	return &storage.PutResult{CID: "fakederiv-cid"}, nil
}

func (f *fakeStore) GetDerivativeCID(_ context.Context, _, _, _ string) (string, bool, error) {
	return f.getDerivResult, f.getDerivFound, nil
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// makeSmallJPEG returns a tiny in-memory JPEG. Used so we can wire up the
// transformer in tests without a fixture file.
func makeSmallJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			img.SetRGBA(x, y, color.RGBA{R: 100, G: 150, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}))
	return buf.Bytes()
}

// newUnitHandler builds a Handler with a fake store and the shared transformer
// (vips.Startup is idempotent).
func newUnitHandler(t *testing.T, fs *fakeStore) *Handler {
	t.Helper()
	tr := getTransformer(t) // initialised once per binary in handler_test.go
	cfg := novaimage.DefaultConfig()
	return New(fs, tr, cfg, nil /* pool unused in unit tests */)
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

// TestPublicTransformOfStagingParentRefused: transform route must return 404
// when the parent is staging, and must never call PutDerivative.
func TestPublicTransformOfStagingParentRefused(t *testing.T) {
	fs := &fakeStore{
		resolveErr: storage.ErrStagingNotVisible,
	}
	h := newUnitHandler(t, fs)
	r := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/i/stagingcid123/w512.webp", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code,
		"staging parent must map to 404, not 500")
	require.Equal(t, 0, fs.putDerivCalls,
		"PutDerivative must never be called for a staging parent (no backdoor)")
}

// TestTransformOverPrunedParentRefetches: when Resolve returns a committed
// BlobView and OpenBytes returns valid image bytes (donor re-fetch succeeded),
// the transform must succeed with 200 and PutDerivative must be called once.
func TestTransformOverPrunedParentRefetches(t *testing.T) {
	jpegBytes := makeSmallJPEG(t)

	fs := &fakeStore{
		resolveResult: &storage.BlobView{
			CID:        "parentcid",
			MIME:       "image/jpeg",
			Product:    "image",
			Visibility: storage.VisibilityPublic,
		},
		openBytesBytes: jpegBytes,
		// GetDerivativeCID returns not-found so we hit generate path.
		getDerivFound: false,
	}
	h := newUnitHandler(t, fs)
	r := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/i/parentcid/w512.webp", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code,
		"committed parent with re-fetched bytes must produce 200")
	require.Equal(t, 1, fs.putDerivCalls,
		"PutDerivative must be called exactly once for a transform miss")
}

// TestPrewarmRunsOnlyAfterCommit: Prewarm must return an error and never call
// PutDerivative when Resolve returns ErrStagingNotVisible. The structural
// guarantee that prewarm is only *enqueued* post-commit is Task 11's
// OnCommitted relocation; this test covers the behavioral abort.
func TestPrewarmRunsOnlyAfterCommit(t *testing.T) {
	fs := &fakeStore{
		resolveErr: storage.ErrStagingNotVisible,
	}
	h := newUnitHandler(t, fs)

	err := h.Prewarm(context.Background(), "stagingcid456", []string{"thumb", "og"})

	require.Error(t, err,
		"Prewarm must return an error for a staging parent")
	require.Equal(t, 0, fs.putDerivCalls,
		"PutDerivative must never be called when the parent is still staging")
}
