package upload_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/upload"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

type fakeCommitter struct {
	calls   atomic.Int32
	lastBuf []byte
}

func (f *fakeCommitter) Put(ctx context.Context, r io.Reader, n int64, pc storage.PutContext) (*storage.PutResult, error) {
	f.calls.Add(1)
	b, _ := io.ReadAll(r)
	f.lastBuf = b
	return &storage.PutResult{CID: "bafytestcid", ByteSize: n, MIME: pc.MIME, Product: pc.Product, Encrypted: true}, nil
}

func mkReader(s string) io.Reader { return strings.NewReader(s) }

func newStore(t *testing.T, ctx context.Context, fc upload.Committer) (*upload.Store, *pgxpool.Pool, string) {
	t.Helper()
	pool := dbtest.New(t, ctx)
	dir := t.TempDir()
	st, err := upload.NewStore(pool, fc, dir, time.Hour, 1024)
	require.NoError(t, err)
	return st, pool, dir
}

func TestIntegrationUploadHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	fc := &fakeCommitter{}
	st, _, dir := newStore(t, ctx, fc)

	id, err := st.Create(ctx, upload.CreateParams{DeclaredLength: 5, MIME: "text/plain", Product: "raw"})
	require.NoError(t, err)

	off, err := st.AppendChunk(ctx, id, 0, mkReader("abc"))
	require.NoError(t, err)
	require.Equal(t, int64(3), off)
	off, err = st.AppendChunk(ctx, id, 3, mkReader("de"))
	require.NoError(t, err)
	require.Equal(t, int64(5), off)

	res, err := st.Finalize(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "bafytestcid", res.CID)
	require.Equal(t, []byte("abcde"), fc.lastBuf)

	// session dir removed after finalize
	_, statErr := os.Stat(filepath.Join(dir, id.String()))
	require.True(t, os.IsNotExist(statErr))

	// idempotent re-finalize: no second Put
	res2, err := st.Finalize(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "bafytestcid", res2.CID)
	require.Equal(t, int32(1), fc.calls.Load())
}

func TestIntegrationUploadOffsetConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	st, _, _ := newStore(t, ctx, &fakeCommitter{})
	id, err := st.Create(ctx, upload.CreateParams{DeclaredLength: 5})
	require.NoError(t, err)
	_, err = st.AppendChunk(ctx, id, 5, mkReader("x")) // wrong offset (expected 0)
	require.ErrorIs(t, err, upload.ErrConflict)
}

func TestIntegrationUploadIncompleteFinalize(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	st, _, _ := newStore(t, ctx, &fakeCommitter{})
	id, err := st.Create(ctx, upload.CreateParams{DeclaredLength: 5})
	require.NoError(t, err)
	_, err = st.AppendChunk(ctx, id, 0, mkReader("ab"))
	require.NoError(t, err)
	_, err = st.Finalize(ctx, id)
	require.ErrorIs(t, err, upload.ErrIncomplete)
}

func TestIntegrationUploadConcurrentPatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	st, _, _ := newStore(t, ctx, &fakeCommitter{})
	id, err := st.Create(ctx, upload.CreateParams{DeclaredLength: 3})
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = st.AppendChunk(ctx, id, 0, mkReader("abc"))
		}(i)
	}
	wg.Wait()

	nils, conflicts := 0, 0
	for _, e := range results {
		switch {
		case e == nil:
			nils++
		case errors.Is(e, upload.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", e)
		}
	}
	require.Equal(t, 1, nils, "exactly one PATCH should win")
	require.Equal(t, 1, conflicts, "the loser should get ErrConflict")
}

func TestIntegrationUploadAbort(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	st, _, dir := newStore(t, ctx, &fakeCommitter{})
	id, err := st.Create(ctx, upload.CreateParams{DeclaredLength: 5})
	require.NoError(t, err)
	require.NoError(t, st.Abort(ctx, id))

	_, err = st.Get(ctx, id)
	require.ErrorIs(t, err, upload.ErrNotFound)
	_, statErr := os.Stat(filepath.Join(dir, id.String()))
	require.True(t, os.IsNotExist(statErr))
}

func TestIntegrationUploadGC(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	st, pool, dir := newStore(t, ctx, &fakeCommitter{})

	var expiredID, finalizedID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO upload_sessions (declared_length, offset_bytes, expires_at, state)
		 VALUES (10, 0, now() - interval '1h', 'in_progress') RETURNING id`).Scan(&expiredID))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, expiredID.String()), 0o700))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO upload_sessions (declared_length, expires_at, state)
		 VALUES (10, now() - interval '1h', 'finalized') RETURNING id`).Scan(&finalizedID))

	n, err := st.GC(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// expired in_progress row + dir gone
	var cnt int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM upload_sessions WHERE id=$1`, expiredID).Scan(&cnt))
	require.Equal(t, 0, cnt)
	_, statErr := os.Stat(filepath.Join(dir, expiredID.String()))
	require.True(t, os.IsNotExist(statErr))

	// finalized row untouched
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM upload_sessions WHERE id=$1`, finalizedID).Scan(&cnt))
	require.Equal(t, 1, cnt)
}
