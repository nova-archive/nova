package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/upload"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	createID   uuid.UUID
	createErr  error
	lastParams upload.CreateParams
	sess       *upload.Session
	getErr     error
	newOff     int64
	appendErr  error
	finRes     *storage.PutResult
	finErr     error
}

func (f *fakeStore) Create(ctx context.Context, p upload.CreateParams) (uuid.UUID, error) {
	f.lastParams = p
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
	res   *storage.PutResult
	err   error
	gotPC storage.PutContext
}

func (f *fakeMP) Put(ctx context.Context, r io.Reader, n int64, pc storage.PutContext) (*storage.PutResult, error) {
	_, _ = io.Copy(io.Discard, r)
	f.gotPC = pc
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
	r.Post("/api/v1/images", h.MultipartImage)
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
	h := NewUploadHandler(&fakeStore{}, &fakeMP{}, 100, false, nil)
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
	h := NewUploadHandler(&fakeStore{createID: id}, &fakeMP{}, 100, false, nil)
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
	h := NewUploadHandler(&fakeStore{newOff: 3}, &fakeMP{}, 100, false, nil)
	rec := do(t, uploadRouter(h), http.MethodPatch, url, bytes.NewReader([]byte("abc")), map[string]string{
		"Upload-Offset": "0", "Content-Type": "text/plain",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Conflict → 409.
	h = NewUploadHandler(&fakeStore{appendErr: upload.ErrConflict}, &fakeMP{}, 100, false, nil)
	rec = do(t, uploadRouter(h), http.MethodPatch, url, bytes.NewReader([]byte("abc")), map[string]string{
		"Upload-Offset": "0", "Content-Type": "application/offset+octet-stream",
	})
	require.Equal(t, http.StatusConflict, rec.Code)

	// Success → 204 + Upload-Offset.
	h = NewUploadHandler(&fakeStore{newOff: 3}, &fakeMP{}, 100, false, nil)
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
	h := NewUploadHandler(&fakeStore{finErr: upload.ErrIncomplete}, &fakeMP{}, 100, false, nil)
	rec := do(t, uploadRouter(h), http.MethodPost, url, nil, nil)
	require.Equal(t, http.StatusConflict, rec.Code)

	// Success → 200 + JSON body.
	h = NewUploadHandler(&fakeStore{finRes: &storage.PutResult{CID: "bafyabc", ByteSize: 5, MIME: "image/png", Product: "raw"}}, &fakeMP{}, 100, false, nil)
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
	h := NewUploadHandler(&fakeStore{}, &fakeMP{res: &storage.PutResult{CID: "bafymp", ByteSize: 3, MIME: "image/png", Product: "raw"}}, 100, false, nil)
	body, ctype := multipartFileBody(t, "abc", "image/png")
	rec := do(t, uploadRouter(h), http.MethodPost, "/api/v1/blobs", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "bafymp")

	// MIME rejected → 400.
	h = NewUploadHandler(&fakeStore{}, &fakeMP{err: storage.ErrMimeRejected}, 100, false, nil)
	body, ctype = multipartFileBody(t, "abc", "image/jpeg")
	rec = do(t, uploadRouter(h), http.MethodPost, "/api/v1/blobs", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Server busy → 503.
	h = NewUploadHandler(&fakeStore{}, &fakeMP{err: storage.ErrServerBusy}, 100, false, nil)
	body, ctype = multipartFileBody(t, "abc", "image/png")
	rec = do(t, uploadRouter(h), http.MethodPost, "/api/v1/blobs", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestMultipartImageSuccess(t *testing.T) {
	mp := &fakeMP{res: &storage.PutResult{CID: "bafyimg", ByteSize: 3, MIME: "image/jpeg", Product: "image"}}
	h := NewUploadHandler(&fakeStore{}, mp, 100, false, nil)
	h.SetImageHooks(
		func(m string) bool { return m == "image/jpeg" },
		func(cid string) map[string]string { return map[string]string{"thumb": "/i/" + cid + "/p/thumb.webp"} },
	)
	body, ctype := multipartFileBody(t, "abc", "image/jpeg")
	rec := do(t, uploadRouter(h), "POST", "/api/v1/images", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "image", mp.gotPC.Product) // product forced to "image"
	var out struct {
		Product string `json:"product"`
		URLs    struct {
			Presets map[string]string `json:"presets"`
		} `json:"urls"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "image", out.Product)
	require.Equal(t, "/i/bafyimg/p/thumb.webp", out.URLs.Presets["thumb"])
}

func TestMultipartImageRejectsNonImage(t *testing.T) {
	mp := &fakeMP{} // res nil; if Put were called it'd panic on nil deref or return nil — but it must NOT be called
	h := NewUploadHandler(&fakeStore{}, mp, 100, false, nil)
	h.SetImageHooks(func(m string) bool { return m == "image/jpeg" }, nil)
	body, ctype := multipartFileBody(t, "hello", "text/plain")
	rec := do(t, uploadRouter(h), "POST", "/api/v1/images", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	require.Equal(t, "", mp.gotPC.Product) // Put never called
}

func TestMultipartImageModerationRejected(t *testing.T) {
	mp := &fakeMP{err: storage.ErrModerationRejected}
	h := NewUploadHandler(&fakeStore{}, mp, 100, false, nil)
	h.SetImageHooks(func(m string) bool { return true }, nil)
	body, ctype := multipartFileBody(t, "abc", "image/jpeg")
	rec := do(t, uploadRouter(h), "POST", "/api/v1/images", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestMultipartSetsOwnerFromIdentity(t *testing.T) {
	t.Parallel()
	mp := &fakeMP{res: &storage.PutResult{CID: "bafyown", ByteSize: 5, MIME: "text/plain", Product: "raw"}}
	h := NewUploadHandler(&fakeStore{}, mp, 1<<20, false, nil)

	body, ctype := multipartFileBody(t, "hello", "text/plain")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/blobs", body)
	r.Header.Set("Content-Type", ctype)
	uid := uuid.New()
	r = r.WithContext(auth.ContextWithIdentity(r.Context(), auth.Identity{UserID: uid.String(), Role: "uploader"}))
	rr := httptest.NewRecorder()
	h.Multipart(rr, r)

	require.Equal(t, http.StatusCreated, rr.Code)
	require.NotNil(t, mp.gotPC.OwnerID)
	require.Equal(t, uid, *mp.gotPC.OwnerID)
}

// TestMultipartRejectsOversizedDeclaredFile confirms the existing
// declared-size check still fires for a well-formed body whose file
// part exceeds maxUploadSize.
func TestMultipartRejectsOversizedDeclaredFile(t *testing.T) {
	t.Parallel()
	mp := &fakeMP{}
	h := NewUploadHandler(&fakeStore{}, mp, 10, false, nil) // 10-byte cap
	body, ctype := multipartFileBody(t, "this is more than ten bytes", "text/plain")
	rec := do(t, uploadRouter(h), http.MethodPost, "/api/v1/blobs", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), "payload_too_large")
	require.Equal(t, "", mp.gotPC.Product, "Put must not be called when the file exceeds the cap")
}

// TestMultipartRejectsBodyExceedingMaxBytesReader confirms the M6.2 B7
// addition: when the raw request body exceeds maxUploadSize + slack,
// http.MaxBytesReader trips ParseMultipartForm with *http.MaxBytesError
// and the handler returns 413. This prevents an attacker who declares
// a small file part but streams a huge body from exhausting disk via
// the multipart spill-to-tmpfile path.
func TestMultipartRejectsBodyExceedingMaxBytesReader(t *testing.T) {
	t.Parallel()
	// maxUploadSize 100 bytes + slack 1 MiB ⇒ effective body cap ≈ 1.0 MiB.
	// Send ~3 MiB of file content to overshoot decisively.
	mp := &fakeMP{}
	h := NewUploadHandler(&fakeStore{}, mp, 100, false, nil)
	const sz = 3 * 1024 * 1024
	body, ctype := multipartFileBody(t, strings.Repeat("a", sz), "text/plain")
	rec := do(t, uploadRouter(h), http.MethodPost, "/api/v1/blobs", body, map[string]string{"Content-Type": ctype})
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), "payload_too_large")
	require.Equal(t, "", mp.gotPC.Product, "Put must not be called when the body exceeds MaxBytesReader")
}

// ---------------------------------------------------------------------------
// Admission cap + token scope tests (Part C / Part E of Task 8)
// ---------------------------------------------------------------------------

// fakeTokenQuerier is a configurable UploadTokenQuerier for handler tests.
type fakeTokenQuerier struct {
	count    int64
	countErr error
	tok      gen.UploadToken
	tokErr   error
}

func (f *fakeTokenQuerier) GetUploadTokenByID(_ context.Context, _ pgtype.UUID) (gen.UploadToken, error) {
	return f.tok, f.tokErr
}

func (f *fakeTokenQuerier) CountActiveSessionsByToken(_ context.Context, _ pgtype.UUID) (int64, error) {
	return f.count, f.countErr
}

// tusHeaders returns the minimum required headers for a CreateTus request.
func tusHeaders(length string, extraMeta ...string) map[string]string {
	h := map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Length": length,
	}
	if len(extraMeta) > 0 {
		h["Upload-Metadata"] = extraMeta[0]
	}
	return h
}

// b64 encodes s as standard base64 (tus Upload-Metadata value format).
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// contextWithTokenIdentity returns a request whose context carries an upload-token
// identity (CredentialID = tokenID.String(), UserID = userID.String()).
func contextWithTokenIdentity(r *http.Request, tokenID, userID uuid.UUID) *http.Request {
	return r.WithContext(auth.ContextWithIdentity(r.Context(), auth.Identity{
		CredentialID: tokenID.String(),
		UserID:       userID.String(),
		Role:         "uploader",
	}))
}

// TestCreateTus_TooManyFiles: when the active-session count for a credential
// meets or exceeds maxFilesPerSession, CreateTus returns 429 too_many_files.
func TestCreateTus_TooManyFiles(t *testing.T) {
	t.Parallel()
	credID := uuid.New()
	userID := uuid.New()
	fs := &fakeStore{createID: uuid.New()}
	ft := &fakeTokenQuerier{count: 1} // count=1 >= maxFilesPerSession=1
	h := NewUploadHandler(fs, &fakeMP{}, 1<<20, false, nil)
	h.SetUploadCaps(16, 4, 1, ft)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	for k, v := range tusHeaders("5") {
		req.Header.Set(k, v)
	}
	req = contextWithTokenIdentity(req, credID, userID)
	rr := httptest.NewRecorder()
	h.CreateTus(rr, req)

	require.Equal(t, http.StatusTooManyRequests, rr.Code)
	require.Equal(t, "2", rr.Header().Get("Retry-After"))
	require.Contains(t, rr.Body.String(), "too_many_files")
}

// TestCreateTus_TooManyConcurrent: when the global admission slot is occupied,
// CreateTus returns 429 too_many_concurrent.
func TestCreateTus_TooManyConcurrent(t *testing.T) {
	t.Parallel()
	ft := &fakeTokenQuerier{count: 0}
	h := NewUploadHandler(&fakeStore{createID: uuid.New()}, &fakeMP{}, 1<<20, false, nil)
	h.SetUploadCaps(1, 1, 100, ft)

	// Occupy the single global slot directly.
	rel, ok := h.adm.TryAcquire("test-occupier")
	require.True(t, ok, "should acquire the only global slot")
	defer rel()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	for k, v := range tusHeaders("5") {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.CreateTus(rr, req)

	require.Equal(t, http.StatusTooManyRequests, rr.Code)
	require.Equal(t, "2", rr.Header().Get("Retry-After"))
	require.Contains(t, rr.Body.String(), "too_many_concurrent")
}

// TestCreateTus_ScopeCollectionConflict: token binds collection X; request
// provides a different collection_id → 403 scope_violation.
func TestCreateTus_ScopeCollectionConflict(t *testing.T) {
	t.Parallel()
	credID := uuid.New()
	userID := uuid.New()
	boundCol := uuid.New()
	otherCol := uuid.New()

	tok := gen.UploadToken{
		CollectionID: pgtype.UUID{Bytes: boundCol, Valid: true},
	}
	ft := &fakeTokenQuerier{count: 0, tok: tok}
	h := NewUploadHandler(&fakeStore{createID: uuid.New()}, &fakeMP{}, 1<<20, false, nil)
	h.SetUploadCaps(16, 4, 100, ft)

	meta := "collection_id " + b64(otherCol.String())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	for k, v := range tusHeaders("5", meta) {
		req.Header.Set(k, v)
	}
	req = contextWithTokenIdentity(req, credID, userID)
	rr := httptest.NewRecorder()
	h.CreateTus(rr, req)

	require.Equal(t, http.StatusForbidden, rr.Code)
	require.Contains(t, rr.Body.String(), "scope_violation")
}

// TestCreateTus_ScopeProductConflict: token binds product "image"; request
// sends product "video" → 403 scope_violation.
func TestCreateTus_ScopeProductConflict(t *testing.T) {
	t.Parallel()
	credID := uuid.New()
	userID := uuid.New()

	tok := gen.UploadToken{
		Product: gen.NullBlobProduct{BlobProduct: gen.BlobProductImage, Valid: true},
	}
	ft := &fakeTokenQuerier{count: 0, tok: tok}
	h := NewUploadHandler(&fakeStore{createID: uuid.New()}, &fakeMP{}, 1<<20, false, nil)
	h.SetUploadCaps(16, 4, 100, ft)

	meta := "product " + b64("video")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	for k, v := range tusHeaders("5", meta) {
		req.Header.Set(k, v)
	}
	req = contextWithTokenIdentity(req, credID, userID)
	rr := httptest.NewRecorder()
	h.CreateTus(rr, req)

	require.Equal(t, http.StatusForbidden, rr.Code)
	require.Contains(t, rr.Body.String(), "scope_violation")
}

// TestCreateTus_ScopeMaxFileSizeExceeded: token binds max_file_size=10;
// request Upload-Length=100 → 413 payload_too_large.
func TestCreateTus_ScopeMaxFileSizeExceeded(t *testing.T) {
	t.Parallel()
	credID := uuid.New()
	userID := uuid.New()

	tok := gen.UploadToken{
		MaxFileSize: pgtype.Int8{Int64: 10, Valid: true},
	}
	ft := &fakeTokenQuerier{count: 0, tok: tok}
	h := NewUploadHandler(&fakeStore{createID: uuid.New()}, &fakeMP{}, 1<<20, false, nil)
	h.SetUploadCaps(16, 4, 100, ft)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	for k, v := range tusHeaders("100") {
		req.Header.Set(k, v)
	}
	req = contextWithTokenIdentity(req, credID, userID)
	rr := httptest.NewRecorder()
	h.CreateTus(rr, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
	require.Contains(t, rr.Body.String(), "payload_too_large")
}

// TestCreateTus_HappyPathWithToken: token binds collection X, request omits
// collection_id. Expects 201, the store CreateParams carry the token ID and
// the bound collection.
func TestCreateTus_HappyPathWithToken(t *testing.T) {
	t.Parallel()
	credID := uuid.New()
	userID := uuid.New()
	sessionID := uuid.New()
	boundCol := uuid.New()

	tok := gen.UploadToken{
		CollectionID: pgtype.UUID{Bytes: boundCol, Valid: true},
	}
	ft := &fakeTokenQuerier{count: 0, tok: tok}
	fs := &fakeStore{createID: sessionID}
	h := NewUploadHandler(fs, &fakeMP{}, 1<<20, false, nil)
	h.SetUploadCaps(16, 4, 100, ft)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	for k, v := range tusHeaders("5") {
		req.Header.Set(k, v)
	}
	req = contextWithTokenIdentity(req, credID, userID)
	rr := httptest.NewRecorder()
	h.CreateTus(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code)
	require.NotNil(t, fs.lastParams.UploadTokenID, "store must receive UploadTokenID")
	require.Equal(t, credID, *fs.lastParams.UploadTokenID, "UploadTokenID must match the credential")
	require.NotNil(t, fs.lastParams.CollectionID, "store must receive bound CollectionID")
	require.Equal(t, boundCol, *fs.lastParams.CollectionID, "CollectionID must be the token-bound one")
}
