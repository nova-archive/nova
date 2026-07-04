package benchcorpus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScratchDSNGuard(t *testing.T) {
	// A non-scratch external DSN is refused outright.
	t.Setenv("BENCH_DATABASE_URL", "postgres://u:p@db.example.test:5432/prod_db")
	t.Setenv("BENCH_ALLOW_NON_SCRATCH", "")
	_, err := resolveBenchPool(t, context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "scratch")

	// The explicit override skips the guard (the connection may still fail,
	// but never with the scratch refusal).
	t.Setenv("BENCH_ALLOW_NON_SCRATCH", "1")
	_, err = resolveBenchPool(t, context.Background())
	if err != nil {
		require.NotContains(t, err.Error(), "scratch")
	}

	// A dbname containing "scratch" passes the guard without the override.
	t.Setenv("BENCH_ALLOW_NON_SCRATCH", "")
	require.NoError(t, checkScratchDSN("postgres://u:p@db.example.test:5432/nova_scratch", false))
}

func TestSeedSkewedCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	t.Setenv("BENCH_DATABASE_URL", "") // testcontainers path (inherently scratch)
	pool, err := resolveBenchPool(t, ctx)
	require.NoError(t, err)

	stats, err := SeedCorpus(ctx, pool, 5_000, 8, 42)
	require.NoError(t, err)

	var blocks int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM blob_blocks`).Scan(&blocks))
	require.EqualValues(t, 5_000, blocks, "BENCH_ROWS is the blob_blocks row count")

	var donors int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM nodes`).Scan(&donors))
	require.Equal(t, 8, donors)

	// The Zipf skew is real, not uniform: the top donor holds ≥ 3× the bottom
	// donor's assignments.
	var top, bottom int64
	require.NoError(t, pool.QueryRow(ctx, `
		WITH per_donor AS (
			SELECT count(*) AS n FROM pin_assignments GROUP BY node_id
		)
		SELECT max(n), min(n) FROM per_donor`).Scan(&top, &bottom))
	require.GreaterOrEqual(t, top, 3*bottom,
		"zipf skew: top donor %d vs bottom donor %d", top, bottom)

	require.NotEmpty(t, stats.DrainingNodeID, "one donor is seeded draining")
	require.NotEmpty(t, stats.SampleCIDs, "seeder exposes sample CIDs for the bench loops")

	// Every blob has replication state; the reconcile queue is partially filled.
	var brs, queue int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM blob_replication_state`).Scan(&brs))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM blob_replication_reconcile_queue`).Scan(&queue))
	require.Greater(t, brs, int64(0))
	require.Greater(t, queue, int64(0))
}
