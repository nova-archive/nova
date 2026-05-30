package storage

import (
	"context"
	"testing"

	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestResolveVisibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want Visibility
	}{
		{"none", nil, VisibilityPrivate},
		{"only private memberships", []string{"private", "private"}, VisibilityPrivate},
		{"unlisted upgrades", []string{"private", "unlisted"}, VisibilityUnlisted},
		{"public wins", []string{"unlisted", "public", "private"}, VisibilityPublic},
		{"single public", []string{"public"}, VisibilityPublic},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveVisibility(c.in); got != c.want {
				t.Fatalf("resolveVisibility(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestIntegrationDerivativeInheritsParentVisibility(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	col := seedCollection(t, ctx, pool, "public", false)

	// Parent original in the public collection.
	_, err := pool.Exec(ctx, `INSERT INTO blobs (cid, mime_type, byte_size, product) VALUES ('bafyparent','image/jpeg',10,'image')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO collection_items (collection_id, blob_cid) VALUES ($1,'bafyparent')`, col)
	require.NoError(t, err)
	// Derivative: parent_cid set, NO collection membership of its own.
	_, err = pool.Exec(ctx, `INSERT INTO blobs (cid, parent_cid, derivative_preset, derivative_format, mime_type, byte_size, product)
		VALUES ('bafyderiv','bafyparent','thumb','webp','image/webp',5,'image')`)
	require.NoError(t, err)

	q := gen.New(pool)
	vis, err := q.ResolveEffectiveVisibility(ctx, "bafyderiv")
	require.NoError(t, err)
	require.Equal(t, []string{"public"}, vis)
}
