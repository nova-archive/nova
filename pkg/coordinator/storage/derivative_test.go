package storage

import (
	"context"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func insertParent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, col uuid.UUID, cidStr string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO blobs (cid, mime_type, byte_size, product) VALUES ($1,'image/jpeg',10,'image')`, cidStr)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO collection_items (collection_id, blob_cid) VALUES ($1,$2)`, col, cidStr)
	require.NoError(t, err)
}

func TestIntegrationPutDerivativeRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := NewService(pool, fb, ks)
	col := seedCollection(t, ctx, pool, "public", false)
	insertParent(t, ctx, pool, col, "bafyparentA")

	persist := func(ctx context.Context, tx pgx.Tx, cid string) error {
		_, err := tx.Exec(ctx, `INSERT INTO image_metadata (cid,width,height,perceptual_hash) VALUES ($1,512,384,NULL)`, cid)
		return err
	}
	out := []byte("transformed-webp-bytes")
	res, err := svc.PutDerivative(ctx, out, DerivativeContext{
		ParentCID: "bafyparentA", Preset: "w512", Format: "webp", MIME: "image/webp", Width: 512, Height: 384,
	}, persist)
	require.NoError(t, err)
	require.True(t, res.Encrypted)

	view, err := svc.Resolve(ctx, res.CID) // inherits parent's public visibility (Task 3 query)
	require.NoError(t, err)
	rc, err := svc.OpenBytes(ctx, view)
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	require.Equal(t, out, got)

	var w, h int
	var ph []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT width,height,perceptual_hash FROM image_metadata WHERE cid=$1`, res.CID).Scan(&w, &h, &ph))
	require.Equal(t, 512, w)
	require.Nil(t, ph)
}

func TestIntegrationPutDerivativeConflictLoserUnpins(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := NewService(pool, fb, ks)
	col := seedCollection(t, ctx, pool, "public", false)
	insertParent(t, ctx, pool, col, "bafyparentB")
	persist := func(ctx context.Context, tx pgx.Tx, cid string) error {
		_, err := tx.Exec(ctx, `INSERT INTO image_metadata (cid,width,height) VALUES ($1,512,384)`, cid)
		return err
	}
	dc := DerivativeContext{ParentCID: "bafyparentB", Preset: "w512", Format: "webp", MIME: "image/webp", Width: 512, Height: 384}

	res1, err := svc.PutDerivative(ctx, []byte("first-bytes"), dc, persist)
	require.NoError(t, err)
	res2, err := svc.PutDerivative(ctx, []byte("second-different-bytes"), dc, persist) // different envelope CID (random nonce), same (parent,preset,format)
	require.NoError(t, err)
	require.Equal(t, res1.CID, res2.CID, "loser returns the winner's CID")
	require.NotEmpty(t, fb.unpinned, "loser unpinned its orphan import")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE parent_cid='bafyparentB'`).Scan(&n))
	require.Equal(t, 1, n)
}
