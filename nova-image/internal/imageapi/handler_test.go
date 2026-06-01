package imageapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multiformats/go-multihash"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	novaimage "github.com/nova-archive/nova/nova-image"
	"github.com/nova-archive/nova/nova-image/internal/transform"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Local test helpers (copies of storage_test internals — not importable).
// ---------------------------------------------------------------------------

// fakeBackend is an in-memory ipfs.Backend for tests.
type fakeBackend struct {
	mu       sync.Mutex
	store    map[string][]byte
	unpinned []string
}

func newFakeBackend() *fakeBackend { return &fakeBackend{store: make(map[string][]byte)} }

func (f *fakeBackend) AddDeterministic(_ context.Context, env []byte) (ipfs.AddResult, error) {
	c, err := cid.V1Builder{Codec: cid.Raw, MhType: multihash.SHA2_256}.Sum(env)
	if err != nil {
		return ipfs.AddResult{}, err
	}
	f.mu.Lock()
	f.store[c.String()] = append([]byte(nil), env...)
	f.mu.Unlock()
	return ipfs.AddResult{
		CID: c, EnvelopeSize: int64(len(env)), Codec: "raw",
		Blocks:     []ipfs.Block{{CID: c, Index: 0, Size: len(env)}},
		MerkleRoot: c,
	}, nil
}

func (f *fakeBackend) Get(_ context.Context, c cid.Cid) (io.ReadCloser, error) {
	f.mu.Lock()
	b, ok := f.store[c.String()]
	f.mu.Unlock()
	if !ok {
		return nil, storage.ErrBlobNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeBackend) Has(_ context.Context, c cid.Cid) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.store[c.String()]
	return ok, nil
}
func (f *fakeBackend) Pin(_ context.Context, _ cid.Cid) error { return nil }
func (f *fakeBackend) Unpin(_ context.Context, c cid.Cid) error {
	f.mu.Lock()
	f.unpinned = append(f.unpinned, c.String())
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) BlockstoreHas(_ context.Context, c cid.Cid) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.store[c.String()]
	return ok, nil
}
func (f *fakeBackend) BlockGet(_ context.Context, c cid.Cid) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.store[c.String()], nil
}
func (f *fakeBackend) Close(_ context.Context) error  { return nil }
func (f *fakeBackend) Health(_ context.Context) error { return nil }

func bootstrapKS(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *envelope.Keystore {
	t.Helper()
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)
	return ks
}

func seedCollection(t *testing.T, ctx context.Context, pool *pgxpool.Pool, visibility string, publicArchival bool) uuid.UUID {
	t.Helper()
	var owner, col uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		uuid.NewString()+"@handler.test").Scan(&owner))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
		 VALUES ($1,'c','c',$2,$3) RETURNING id`, owner, visibility, publicArchival).Scan(&col))
	return col
}

// makeJPEG produces a real JPEG from a solid-colour image of the given size.
func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{R: 100, G: 150, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}))
	return buf.Bytes()
}

// isWebP checks for RIFF....WEBP magic bytes.
func isWebP(b []byte) bool {
	if len(b) < 12 {
		return false
	}
	// RIFF{4-byte size}WEBP
	riff := binary.LittleEndian.Uint32(b[0:4])
	_ = riff
	return b[0] == 'R' && b[1] == 'I' && b[2] == 'F' && b[3] == 'F' &&
		b[8] == 'W' && b[9] == 'E' && b[10] == 'B' && b[11] == 'P'
}

// newRouter builds a chi.Router with the handler's routes registered.
func newRouter(h *Handler) chi.Router {
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	return r
}

// onceTransformer is a package-level transformer initialised once per test
// binary (vips.Startup is idempotent).
var (
	onceTransformer sync.Once
	sharedTR        *transform.Transformer
)

func getTransformer(t *testing.T) *transform.Transformer {
	t.Helper()
	onceTransformer.Do(func() {
		require.NoError(t, transform.Startup(0))
		sharedTR = transform.New(transform.Bounds{MaxMegapixels: 100, MaxConcurrent: 4})
	})
	return sharedTR
}

// ---------------------------------------------------------------------------
// Integration tests — require a running Postgres + libvips.
// ---------------------------------------------------------------------------

// TestIntegrationTransformExitCriterion is the M5 exit criterion.
// Upload a JPEG parent into a PUBLIC collection; GET /i/{cid}/w512.webp →
// 200, body has WEBP magic; exactly ONE derivative row.
// Then GET again → 200, still exactly ONE row (cache hit).
func TestIntegrationTransformExitCriterion(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := storage.NewService(pool, fb, ks)
	col := seedCollection(t, ctx, pool, "public", false)

	// Seed a JPEG image parent.
	jpegBytes := makeJPEG(t, 1024, 768)
	res, err := svc.Put(ctx, bytes.NewReader(jpegBytes), int64(len(jpegBytes)),
		storage.PutContext{MIME: "image/jpeg", Product: "image", CollectionID: &col})
	require.NoError(t, err)
	parentCID := res.CID

	// Seed image_metadata row for the parent (required for serveOriginal header, not for transform).
	_, err = pool.Exec(ctx,
		`INSERT INTO image_metadata (cid, width, height) VALUES ($1, 1024, 768)`, parentCID)
	require.NoError(t, err)

	tr := getTransformer(t)
	cfg := novaimage.DefaultConfig()
	h := New(svc, tr, cfg, pool)
	r := newRouter(h)

	// First request — cache miss → generate.
	req := httptest.NewRequest(http.MethodGet, "/i/"+parentCID+"/w512.webp", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "image/webp", w.Header().Get("Content-Type"))
	body := w.Body.Bytes()
	require.True(t, isWebP(body), "expected WEBP magic in response body")
	require.NotEmpty(t, w.Header().Get("X-Nova-Cid"), "X-Nova-Cid must be set")

	// Exactly one derivative row.
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blobs WHERE parent_cid=$1 AND derivative_preset='w512' AND derivative_format='webp'`,
		parentCID).Scan(&n))
	require.Equal(t, 1, n, "must be exactly one derivative row after first request")

	// Second request — cache hit.
	req2 := httptest.NewRequest(http.MethodGet, "/i/"+parentCID+"/w512.webp", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	require.Equal(t, http.StatusOK, w2.Code)
	require.True(t, isWebP(w2.Body.Bytes()), "second request must also return WEBP")

	// Still exactly one row.
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blobs WHERE parent_cid=$1 AND derivative_preset='w512' AND derivative_format='webp'`,
		parentCID).Scan(&n))
	require.Equal(t, 1, n, "cache hit must not create a second derivative row")
}

// TestIntegrationPresetAndNegatives covers preset hit, 400/406/415/401 negative paths.
func TestIntegrationPresetAndNegatives(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := storage.NewService(pool, fb, ks)
	tr := getTransformer(t)
	cfg := novaimage.DefaultConfig()
	h := New(svc, tr, cfg, pool)
	r := newRouter(h)

	// --- preset hit ---
	pubCol := seedCollection(t, ctx, pool, "public", false)
	jpegBytes := makeJPEG(t, 512, 512)
	res, err := svc.Put(ctx, bytes.NewReader(jpegBytes), int64(len(jpegBytes)),
		storage.PutContext{MIME: "image/jpeg", Product: "image", CollectionID: &pubCol})
	require.NoError(t, err)
	imgCID := res.CID

	t.Run("preset thumb.webp 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/i/"+imgCID+"/p/thumb.webp", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		require.True(t, isWebP(w.Body.Bytes()), "expected WEBP from preset")
	})

	t.Run("w999.webp 400 dimension not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/i/"+imgCID+"/w999.webp", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("w512.avif 406 format not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/i/"+imgCID+"/w512.avif", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotAcceptable, w.Code)
	})

	// --- non-image Product → 415 ---
	rawCol := seedCollection(t, ctx, pool, "public", false)
	rawData := []byte("not an image")
	rawRes, err := svc.Put(ctx, bytes.NewReader(rawData), int64(len(rawData)),
		storage.PutContext{MIME: "text/plain", Product: "raw", CollectionID: &rawCol})
	require.NoError(t, err)

	t.Run("non-image product 415", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/i/"+rawRes.CID+"/w512.webp", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnsupportedMediaType, w.Code)
	})

	// --- private collection → 401 ---
	privCol := seedCollection(t, ctx, pool, "private", false)
	privBytes := makeJPEG(t, 200, 200)
	privRes, err := svc.Put(ctx, bytes.NewReader(privBytes), int64(len(privBytes)),
		storage.PutContext{MIME: "image/jpeg", Product: "image", CollectionID: &privCol})
	require.NoError(t, err)

	t.Run("private collection 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/i/"+privRes.CID+"/w512.webp", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// TestIntegrationSingleFlightOneRow fires 8 concurrent GET /i/{cid}/p/thumb.webp
// and verifies exactly ONE derivative row afterwards.
func TestIntegrationSingleFlightOneRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := storage.NewService(pool, fb, ks, storage.WithWriteLimits(104857600, 16))
	tr := getTransformer(t)
	cfg := novaimage.DefaultConfig()
	h := New(svc, tr, cfg, pool)
	r := newRouter(h)

	pubCol := seedCollection(t, ctx, pool, "public", false)
	jpegBytes := makeJPEG(t, 800, 600)
	res, err := svc.Put(ctx, bytes.NewReader(jpegBytes), int64(len(jpegBytes)),
		storage.PutContext{MIME: "image/jpeg", Product: "image", CollectionID: &pubCol})
	require.NoError(t, err)
	parentCID := res.CID

	const concurrency = 8
	var wg sync.WaitGroup
	statuses := make([]int, concurrency)
	for i := range concurrency {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/i/"+parentCID+"/p/thumb.webp", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			statuses[idx] = w.Code
		}(i)
	}
	wg.Wait()

	for i, s := range statuses {
		require.Equal(t, http.StatusOK, s, "goroutine %d got status %d", i, s)
	}

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blobs WHERE parent_cid=$1 AND derivative_preset='thumb' AND derivative_format='webp'`,
		parentCID).Scan(&n))
	require.Equal(t, 1, n, "single-flight must produce exactly one derivative row")
}

// TestIntegrationOriginalRoute exercises /i/{cid} bare (serve original).
func TestIntegrationOriginalRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := storage.NewService(pool, fb, ks)
	pubCol := seedCollection(t, ctx, pool, "public", false)

	jpegBytes := makeJPEG(t, 300, 200)
	res, err := svc.Put(ctx, bytes.NewReader(jpegBytes), int64(len(jpegBytes)),
		storage.PutContext{MIME: "image/jpeg", Product: "image", CollectionID: &pubCol})
	require.NoError(t, err)
	parentCID := res.CID

	_, err = pool.Exec(ctx,
		`INSERT INTO image_metadata (cid, width, height) VALUES ($1, 300, 200)`, parentCID)
	require.NoError(t, err)

	tr := getTransformer(t)
	cfg := novaimage.DefaultConfig()
	h := New(svc, tr, cfg, pool)
	r := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/i/"+parentCID, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "image/jpeg", w.Header().Get("Content-Type"))
	require.Equal(t, strconv.Itoa(len(jpegBytes)), w.Header().Get("Content-Length"))
	require.Equal(t, "300", w.Header().Get("X-Nova-Width"))
	require.Equal(t, "200", w.Header().Get("X-Nova-Height"))
}

// TestIntegrationJSONRoute exercises /i/{cid}.json.
func TestIntegrationJSONRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := storage.NewService(pool, fb, ks)
	pubCol := seedCollection(t, ctx, pool, "public", false)

	jpegBytes := makeJPEG(t, 100, 100)
	res, err := svc.Put(ctx, bytes.NewReader(jpegBytes), int64(len(jpegBytes)),
		storage.PutContext{MIME: "image/jpeg", Product: "image", CollectionID: &pubCol})
	require.NoError(t, err)
	parentCID := res.CID

	_, err = pool.Exec(ctx,
		`INSERT INTO image_metadata (cid, width, height) VALUES ($1, 100, 100)`, parentCID)
	require.NoError(t, err)

	tr := getTransformer(t)
	cfg := novaimage.DefaultConfig()
	h := New(svc, tr, cfg, pool)
	r := newRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/i/"+parentCID+".json", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	require.Contains(t, w.Body.String(), `"cid"`)
	require.Contains(t, w.Body.String(), `"width"`)
}
