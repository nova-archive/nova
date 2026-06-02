package integrity

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func partitionExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = $1`, name).Scan(&n))
	return n > 0
}

func TestMaintainer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping maintainer DB test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	clk := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	m := NewMaintainer(pool, 30*24*time.Hour, 365*24*time.Hour, nil,
		WithMaintClock(func() time.Time { return clk }))

	t.Run("creates current + lookahead partitions", func(t *testing.T) {
		require.NoError(t, m.ensurePartitions(ctx))
		// 0003 ships 2026_06; create-ahead must add July (the insert cliff) + August.
		require.True(t, partitionExists(t, ctx, pool, "integrity_audits_2026_07"))
		require.True(t, partitionExists(t, ctx, pool, "integrity_audits_2026_08"))
	})

	t.Run("future-dated insert succeeds after create-ahead", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO integrity_audits (cid, audit_kind, result, audited_at)
			 VALUES ('bafyJuly','kubo_pin_present','pass','2026-07-10 00:00:00+00')`)
		require.NoError(t, err)
	})

	t.Run("prunes old passes, keeps old failures", func(t *testing.T) {
		const old = "2026-05-06 00:00:00+00" // 40d before the clock ⇒ default partition
		_, err := pool.Exec(ctx,
			`INSERT INTO integrity_audits (cid, audit_kind, result, audited_at)
			 VALUES ('bafyOldPass','envelope_decode','pass',$1)`, old)
		require.NoError(t, err)
		_, err = pool.Exec(ctx,
			`INSERT INTO integrity_audits (cid, audit_kind, result, error, audited_at)
			 VALUES ('bafyOldFail','envelope_decode','fail','boom',$1)`, old)
		require.NoError(t, err)

		require.NoError(t, m.prunePasses(ctx))

		var passCount, failCount int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM integrity_audits WHERE cid='bafyOldPass'`).Scan(&passCount))
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM integrity_audits WHERE cid='bafyOldFail'`).Scan(&failCount))
		require.Equal(t, 0, passCount, "old pass should be pruned")
		require.Equal(t, 1, failCount, "old failure should be retained")
	})

	t.Run("drops partitions older than fail retention", func(t *testing.T) {
		// Monthly partitions only exist for 2026-06+ (earlier ranges are covered
		// by integrity_audits_default), so use a far-future clock to age the real
		// create-ahead partitions past the 1-year fail-retention window.
		future := time.Date(2028, 1, 15, 0, 0, 0, 0, time.UTC)
		mFuture := NewMaintainer(pool, 30*24*time.Hour, 365*24*time.Hour, nil,
			WithMaintClock(func() time.Time { return future }))
		// A partition recent relative to 2028 that must survive.
		_, err := pool.Exec(ctx,
			`CREATE TABLE IF NOT EXISTS integrity_audits_2028_01 PARTITION OF integrity_audits
			 FOR VALUES FROM ('2028-01-01 00:00:00+00') TO ('2028-02-01 00:00:00+00')`)
		require.NoError(t, err)
		require.True(t, partitionExists(t, ctx, pool, "integrity_audits_2026_07"), "exists from create-ahead")

		require.NoError(t, mFuture.dropAgedPartitions(ctx))

		require.False(t, partitionExists(t, ctx, pool, "integrity_audits_2026_07"), "2026 partition aged out by 2028 clock")
		require.True(t, partitionExists(t, ctx, pool, "integrity_audits_2028_01"), "recent partition survives")
		require.True(t, partitionExists(t, ctx, pool, "integrity_audits_default"), "default catch-all is never dropped")
	})
}
