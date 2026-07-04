package integrity

import (
	"context"
	"fmt"
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

// TestMaintainerJobsPartitions covers P2-M7.1 (D-M7.1-2): the Maintainer also
// create-aheads jobs' monthly partitions — the "partition-rotation job" that
// 0002_jobs.sql promised. Create-ahead only: the job reaper owns row
// lifecycle, so maintain() must never prune or drop jobs partitions.
func TestMaintainerJobsPartitions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping maintainer DB test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	// Clock 4 months past real now: migration 0017 provisions install-month +2
	// at migrate time, so a clock inside that window would pass without the
	// Maintainer. Month X = now+4 is provably the Maintainer's work, whatever
	// the real date is when this test runs.
	clk := monthStart(time.Now()).AddDate(0, 4, 14)
	m := NewMaintainer(pool, 30*24*time.Hour, 365*24*time.Hour, nil,
		WithMaintClock(func() time.Time { return clk }))

	names := make([]string, 0, 3)
	for i := 0; i <= 2; i++ {
		mo := monthStart(clk).AddDate(0, i, 0)
		names = append(names, fmt.Sprintf("jobs_%04d_%02d", mo.Year(), int(mo.Month())))
	}
	for _, name := range names {
		require.False(t, partitionExists(t, ctx, pool, name),
			"%s must not exist before maintain (0017 only provisions install-month+2)", name)
	}

	m.maintain(ctx)

	for _, name := range names {
		require.True(t, partitionExists(t, ctx, pool, name),
			"maintain must create-ahead %s (month X..X+2)", name)
	}

	// An insert dated in month X lands in the fresh partition.
	_, err := pool.Exec(ctx,
		`INSERT INTO jobs (kind, created_at) VALUES ('noop', $1)`, clk)
	require.NoError(t, err, "jobs insert in month X must succeed after create-ahead")
}

// TestMaintainerAuditLogPartitions covers M9: the Maintainer also create-aheads
// audit_log's monthly partitions (the committed ones stop at 2026-07-01) and
// never prunes audit_log (operator-action history is retained for years).
func TestMaintainerAuditLogPartitions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping maintainer DB test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	clk := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	m := NewMaintainer(pool, 30*24*time.Hour, 365*24*time.Hour, nil,
		WithMaintClock(func() time.Time { return clk }))

	// An old operator-action row that must never be pruned (legal retention).
	_, err := pool.Exec(ctx,
		`INSERT INTO audit_log (action, target_type, target_id, at)
		 VALUES ('test.action','cid','bafyX','2020-01-01 00:00:00+00')`)
	require.NoError(t, err)

	// A full maintain cycle now also create-aheads audit_log (and must not prune it).
	m.maintain(ctx)

	require.True(t, partitionExists(t, ctx, pool, "audit_log_2026_07"), "audit_log create-ahead provisions next month")
	require.True(t, partitionExists(t, ctx, pool, "audit_log_2026_08"))

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action='test.action'`).Scan(&n))
	require.Equal(t, 1, n, "audit_log is never pruned (legal retention)")
}
