package imagemeta

import (
	"context"
	"testing"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestIntegrationInsertAndGet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	_, err := pool.Exec(ctx, `INSERT INTO blobs (cid, mime_type, byte_size, product) VALUES ('bafyimg','image/webp',9,'image')`)
	require.NoError(t, err)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, Insert(ctx, tx, "bafyimg", 512, 384, nil, nil))
	require.NoError(t, tx.Commit(ctx))

	m, err := Get(ctx, pool, "bafyimg")
	require.NoError(t, err)
	require.Equal(t, 512, m.Width)
	require.Equal(t, 384, m.Height)

	// perceptual_hash is NULL in Phase 1.
	var ph []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT perceptual_hash FROM image_metadata WHERE cid='bafyimg'`).Scan(&ph))
	require.Nil(t, ph)
}
