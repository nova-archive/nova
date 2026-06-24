package handlers_test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

type fakeReader struct {
	view    *storage.BlobView
	err     error
	body    string
	openErr error // if non-nil, OpenBytes returns it (Resolve still succeeds)
}

func (f fakeReader) Resolve(_ context.Context, _ string) (*storage.BlobView, error) {
	return f.view, f.err
}
func (f fakeReader) OpenBytes(_ context.Context, _ *storage.BlobView) (io.ReadCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func route(h *handlers.BlobHandler) chi.Router {
	r := chi.NewRouter()
	r.Get("/blob/{cid}", h.Serve)
	r.Head("/blob/{cid}", h.Head)
	return r
}

func TestBlobGetPublicEncrypted(t *testing.T) {
	t.Parallel()
	view := &storage.BlobView{CID: "bafyX", MIME: "text/plain", PlaintextSize: 5,
		EnvelopeVersion: 1, Visibility: storage.VisibilityPublic, Encrypted: true, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view, body: "hello"})
	rec := httptest.NewRecorder()
	route(h).ServeHTTP(rec, httptest.NewRequest("GET", "/blob/bafyX", nil))

	require.Equal(t, 200, rec.Code)
	require.Equal(t, "hello", rec.Body.String())
	require.Equal(t, "text/plain", rec.Header().Get("Content-Type"))
	require.Equal(t, `"bafyX"`, rec.Header().Get("ETag"))
	require.Equal(t, "bafyX", rec.Header().Get("X-Nova-Cid"))
	require.Equal(t, "1", rec.Header().Get("X-Nova-Envelope-Version"))
	require.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
}

func TestBlobGetUnlistedCacheHeader(t *testing.T) {
	t.Parallel()
	view := &storage.BlobView{CID: "bafyU", MIME: "text/plain", PlaintextSize: 2,
		EnvelopeVersion: 1, Visibility: storage.VisibilityUnlisted, Encrypted: true, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view, body: "hi"})
	rec := httptest.NewRecorder()
	route(h).ServeHTTP(rec, httptest.NewRequest("GET", "/blob/bafyU", nil))
	require.Equal(t, "private, max-age=300, must-revalidate", rec.Header().Get("Cache-Control"))
}

func TestBlobRangeOnEncrypted416(t *testing.T) {
	t.Parallel()
	view := &storage.BlobView{CID: "bafyX", MIME: "text/plain", Encrypted: true,
		Visibility: storage.VisibilityPublic, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view, body: "hello"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/blob/bafyX", nil)
	req.Header.Set("Range", "bytes=0-1")
	route(h).ServeHTTP(rec, req)
	require.Equal(t, 416, rec.Code)
}

func TestBlobHeadNoBody(t *testing.T) {
	t.Parallel()
	view := &storage.BlobView{CID: "bafyX", MIME: "text/plain", PlaintextSize: 5,
		EnvelopeVersion: 1, Visibility: storage.VisibilityPublic, Encrypted: true, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view, body: "hello"})
	rec := httptest.NewRecorder()
	route(h).ServeHTTP(rec, httptest.NewRequest("HEAD", "/blob/bafyX", nil))
	require.Equal(t, 200, rec.Code)
	require.Empty(t, rec.Body.String())
	require.Equal(t, "5", rec.Header().Get("Content-Length"))
}

func TestBlobStatusMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		bytesCode int
		jsonCode  int
	}{
		{"not found", storage.ErrBlobNotFound, 404, 404},
		{"auth required", storage.ErrBlobAuthRequired, 401, 404},
		{"quarantined", storage.ErrBlobQuarantined, 451, 404},
		{"soft deleted", storage.ErrBlobSoftDeleted, 410, 410},
		{"tombstoned", storage.ErrBlobTombstoned, 410, 410},
		{"key shredded", storage.ErrKeyShredded, 410, 410},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := handlers.NewBlobHandler(fakeReader{err: c.err})
			rb := httptest.NewRecorder()
			route(h).ServeHTTP(rb, httptest.NewRequest("GET", "/blob/x", nil))
			require.Equal(t, c.bytesCode, rb.Code)
			rj := httptest.NewRecorder()
			route(h).ServeHTTP(rj, httptest.NewRequest("GET", "/blob/x.json", nil))
			require.Equal(t, c.jsonCode, rj.Code)
		})
	}
}

// TestServeBytesUnavailable503 exercises the OpenBytes (post-Resolve) error
// path: Resolve succeeds (the blob is committed) but OpenBytes returns
// ErrNoSourceableHolder because no donor can momentarily serve it. The handler
// must route that through mapBytesError to a 503, not a hard-coded 500.
func TestServeBytesUnavailable503(t *testing.T) {
	t.Parallel()
	view := &storage.BlobView{CID: "bafyN", MIME: "text/plain", PlaintextSize: 5,
		EnvelopeVersion: 1, Visibility: storage.VisibilityPublic, Encrypted: false, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view, openErr: storage.ErrNoSourceableHolder})
	rec := httptest.NewRecorder()
	route(h).ServeHTTP(rec, httptest.NewRequest("GET", "/blob/bafyN", nil))
	require.Equal(t, 503, rec.Code)
}

func TestBlobPlaintextRange206(t *testing.T) {
	t.Parallel()
	view := &storage.BlobView{CID: "bafyP", MIME: "text/plain", PlaintextSize: 5,
		EnvelopeVersion: 1, Visibility: storage.VisibilityPublic, Encrypted: false, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view, body: "hello"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/blob/bafyP", nil)
	req.Header.Set("Range", "bytes=0-1")
	route(h).ServeHTTP(rec, req)
	require.Equal(t, 206, rec.Code)
	require.Equal(t, "he", rec.Body.String())
	require.Equal(t, "bytes 0-1/5", rec.Header().Get("Content-Range"))
}

func TestBlobJSONPublic(t *testing.T) {
	t.Parallel()
	owner := "11111111-1111-1111-1111-111111111111"
	view := &storage.BlobView{CID: "bafyX", MIME: "image/png", PlaintextSize: 99,
		Product: "raw", OwnerID: &owner, Visibility: storage.VisibilityPublic, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view})
	rec := httptest.NewRecorder()
	route(h).ServeHTTP(rec, httptest.NewRequest("GET", "/blob/bafyX.json", nil))
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"cid":"bafyX"`)
	require.Contains(t, rec.Body.String(), `"byte_size":99`)
	require.Contains(t, rec.Body.String(), `"state":"active"`)
}
