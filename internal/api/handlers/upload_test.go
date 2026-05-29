package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/upload"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	createID  uuid.UUID
	createErr error
	sess      *upload.Session
	getErr    error
	newOff    int64
	appendErr error
	finRes    *storage.PutResult
	finErr    error
}

func (f *fakeStore) Create(ctx context.Context, p upload.CreateParams) (uuid.UUID, error) {
	return f.createID, f.createErr
}
func (f *fakeStore) Get(ctx context.Context, id uuid.UUID) (*upload.Session, error) {
	return f.sess, f.getErr
}
func (f *fakeStore) AppendChunk(ctx context.Context, id uuid.UUID, off int64, r io.Reader) (int64, error) {
	_, _ = io.Copy(io.Discard, r)
	return f.newOff, f.appendErr
}
func (f *fakeStore) Finalize(ctx context.Context, id uuid.UUID) (*storage.PutResult, error) {
	return f.finRes, f.finErr
}
func (f *fakeStore) Abort(ctx context.Context, id uuid.UUID) error { return nil }

type fakeMP struct {
	res *storage.PutResult
	err error
}

func (f *fakeMP) Put(ctx context.Context, r io.Reader, n int64, pc storage.PutContext) (*storage.PutResult, error) {
	_, _ = io.Copy(io.Discard, r)
	return f.res, f.err
}

func uploadRouter(h *UploadHandler) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Post("/api/v1/uploads", h.CreateTus)
	r.Route("/api/v1/uploads/{id}", func(r chi.Router) {
		r.Head("/", h.HeadTus)
		r.Patch("/", h.PatchTus)
		r.Delete("/", h.DeleteTus)
		r.Post("/finalize", h.FinalizeTus)
	})
	r.Post("/api/v1/blobs", h.Multipart)
	return r
}

func do(t *testing.T, router http.Handler, method, url string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestParseUploadMetadata(t *testing.T) {
	// "filename"=ZmlsZQ== ("file"), "mime_type"=aW1hZ2UvanBlZw== ("image/jpeg")
	got := parseUploadMetadata("filename ZmlsZQ==,mime_type aW1hZ2UvanBlZw==")
	require.Equal(t, "file", got["filename"])
	require.Equal(t, "image/jpeg", got["mime_type"])
	require.Empty(t, parseUploadMetadata("")["mime_type"])
}

func TestCreateTusValidation(t *testing.T) {
	h := NewUploadHandler(&fakeStore{}, &fakeMP{}, 100, false)
	router := uploadRouter(h)

	// Missing Tus-Resumable → 400.
	rec := do(t, router, http.MethodPost, "/api/v1/uploads", nil, map[string]string{"Upload-Length": "5"})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Oversize Upload-Length → 413.
	rec = do(t, router, http.MethodPost, "/api/v1/uploads", nil, map[string]string{
		"Tus-Resumable": "1.0.0", "Upload-Length": "999",
	})
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestCreateTusSuccess(t *testing.T) {
	id := uuid.New()
	h := NewUploadHandler(&fakeStore{createID: id}, &fakeMP{}, 100, false)
	rec := do(t, uploadRouter(h), http.MethodPost, "/api/v1/uploads", nil, map[string]string{
		"Tus-Resumable": "1.0.0", "Upload-Length": "5",
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "/api/v1/uploads/"+id.String(), rec.Header().Get("Location"))
	require.Equal(t, "0", rec.Header().Get("Upload-Offset"))
}

func TestPatchTus(t *testing.T) {
	id := uuid.New()
	url := "/api/v1/uploads/" + id.String()

	// Wrong content-type → 400.
	h := NewUploadHandler(&fakeStore{newOff: 3}, &fakeMP{}, 100, false)
	rec := do(t, uploadRouter(h), http.MethodPatch, url, bytes.NewReader([]byte("abc")), map[string]string{
		"Upload-Offset": "0", "Content-Type": "text/plain",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Conflict → 409.
	h = NewUploadHandler(&fakeStore{appendErr: upload.ErrConflict}, &fakeMP{}, 100, false)
	rec = do(t, uploadRouter(h), http.MethodPatch, url, bytes.NewReader([]byte("abc")), map[string]string{
		"Upload-Offset": "0", "Content-Type": "application/offset+octet-stream",
	})
	require.Equal(t, http.StatusConflict, rec.Code)

	// Success → 204 + Upload-Offset.
	h = NewUploadHandler(&fakeStore{newOff: 3}, &fakeMP{}, 100, false)
	rec = do(t, uploadRouter(h), http.MethodPatch, url, bytes.NewReader([]byte("abc")), map[string]string{
		"Upload-Offset": "0", "Content-Type": "application/offset+octet-stream",
	})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "3", rec.Header().Get("Upload-Offset"))
}

func TestFinalizeTus(t *testing.T) {
	id := uuid.New()
	url := "/api/v1/uploads/" + id.String() + "/finalize"

	// Incomplete → 409.
	h := NewUploadHandler(&fakeStore{finErr: upload.ErrIncomplete}, &fakeMP{}, 100, false)
	rec := do(t, uploadRouter(h), http.MethodPost, url, nil, nil)
	require.Equal(t, http.StatusConflict, rec.Code)

	// Success → 200 + JSON body.
	h = NewUploadHandler(&fakeStore{finRes: &storage.PutResult{CID: "bafyabc", ByteSize: 5, MIME: "image/png", Product: "raw"}}, &fakeMP{}, 100, false)
	rec = do(t, uploadRouter(h), http.MethodPost, url, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "bafyabc", body["cid"])
	urls := body["urls"].(map[string]any)
	require.Equal(t, "/blob/bafyabc", urls["original"])
}

func multipartFileBody(t *testing.T, content, ctype string) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="x.png"`)
	if ctype != "" {
		hdr.Set("Content-Type", ctype)
	}
	part, err := w.CreatePart(hdr)
	require.NoError(t, err)
	_, _ = part.Write([]byte(content))
	require.NoError(t, w.Close())
	return &b, w.FormDataContentType()
}

func TestMultipart(t *testing.T) {
	// Success → 201 + JSON.
	h := NewUploadHandler(&fakeStore{}, &fakeMP{res: &storage.PutResult{CID: "bafymp", ByteSize: 3, MIME: "image/png", Product: "raw"}}, 100, false)
	body, ctype := multipartFileBody(t, "abc", "image/png")
	rec := do(t, uploadRouter(h), http.MethodPost, "/api/v1/blobs", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "bafymp")

	// MIME rejected → 400.
	h = NewUploadHandler(&fakeStore{}, &fakeMP{err: storage.ErrMimeRejected}, 100, false)
	body, ctype = multipartFileBody(t, "abc", "image/jpeg")
	rec = do(t, uploadRouter(h), http.MethodPost, "/api/v1/blobs", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Server busy → 503.
	h = NewUploadHandler(&fakeStore{}, &fakeMP{err: storage.ErrServerBusy}, 100, false)
	body, ctype = multipartFileBody(t, "abc", "image/png")
	rec = do(t, uploadRouter(h), http.MethodPost, "/api/v1/blobs", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
