package storage

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

type fakeHook struct {
	transformTo []byte
	mime        string
	action      ScanAction
	committed   []string
}

func (h *fakeHook) Analyze(ctx context.Context, pc PutContext, plaintext []byte) (AnalyzeResult, error) {
	ar := AnalyzeResult{Scan: ScanResult{Action: h.action}}
	if h.transformTo != nil {
		ar.Transformed = h.transformTo
		ar.ResultMIME = h.mime
	}
	ar.Persist = func(ctx context.Context, tx pgx.Tx, cid string) error {
		_, err := tx.Exec(ctx, `INSERT INTO image_metadata (cid,width,height) VALUES ($1,1,1)`, cid)
		return err
	}
	return ar, nil
}
func (h *fakeHook) OnCommitted(ctx context.Context, ref CommittedRef) {
	h.committed = append(h.committed, ref.CID)
}

// minimalPNG is a minimal PNG magic-byte prefix that passes http.DetectContentType.
var minimalPNG = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'I', 'H', 'D', 'R'}

func TestIntegrationPutHookTransformAndPersist(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	h := &fakeHook{transformTo: []byte("webp!"), mime: "image/webp", action: ActionAllow}
	svc := NewService(pool, fb, ks, WithProductHook(h))
	col := seedCollection(t, ctx, pool, "public", false)

	res, err := svc.Put(ctx, bytes.NewReader(minimalPNG), int64(len(minimalPNG)),
		PutContext{MIME: "image/png", Product: "image", CollectionID: &col})
	require.NoError(t, err)
	require.Equal(t, "image/webp", res.MIME, "stored mime is the transformed one")
	view, err := svc.Resolve(ctx, res.CID)
	require.NoError(t, err)
	rc, err := svc.OpenBytes(ctx, view)
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	require.Equal(t, []byte("webp!"), got, "stored bytes are the transformed ones")
	require.Equal(t, []string{res.CID}, h.committed)

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM image_metadata WHERE cid=$1`, res.CID).Scan(&n))
	require.Equal(t, 1, n)
}

func TestIntegrationPutHookModerationReject(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	h := &fakeHook{action: ActionTombstone}
	svc := NewService(pool, fb, ks, WithProductHook(h))
	col := seedCollection(t, ctx, pool, "public", false)
	_, err := svc.Put(ctx, bytes.NewReader(minimalPNG), int64(len(minimalPNG)), PutContext{MIME: "image/png", Product: "image", CollectionID: &col})
	require.ErrorIs(t, err, ErrModerationRejected)
	require.Empty(t, fb.store, "nothing imported on reject")
}
