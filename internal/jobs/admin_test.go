package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/jobs"
)

// insertJob writes one jobs row in a chosen state/kind with an explicit
// created_at offset (seconds in the past) so the tests can assert newest-first
// ordering deterministically. offsetSecs stays small to remain inside the
// current monthly partition.
func insertJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, kind, state string, attempts int, lastErr string, offsetSecs int) uuid.UUID {
	t.Helper()
	var le *string
	if lastErr != "" {
		le = &lastErr
	}
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO jobs (kind, payload, state, attempts, max_attempts, last_error, created_at, not_before)
		 VALUES ($1, '{}'::jsonb, $2::job_state, $3, 5, $4, now() - make_interval(secs => $5), now())
		 RETURNING id`,
		kind, state, attempts, le, offsetSecs).Scan(&id))
	return id
}

func TestAdminStoreListCountFilterPaginate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	store := jobs.NewAdminStore(pool)

	// Newest (offset 0) → oldest (offset 3).
	newest := insertJob(t, ctx, pool, "derivative_prewarm", "pending", 0, "", 0)
	insertJob(t, ctx, pool, "derivative_prewarm", "completed", 1, "", 1)
	dead := insertJob(t, ctx, pool, "derivative_prewarm", "dead", 5, "kubo pin timeout", 2)
	oldest := insertJob(t, ctx, pool, "webhook_emit", "pending", 0, "", 3)

	// List all, newest-first.
	all, err := store.List(ctx, jobs.Filter{}, 50, 0)
	require.NoError(t, err)
	require.Len(t, all, 4)
	require.Equal(t, newest, all[0].ID, "newest first")
	require.Equal(t, oldest, all[3].ID, "oldest last")

	n, err := store.Count(ctx, jobs.Filter{})
	require.NoError(t, err)
	require.EqualValues(t, 4, n)

	// Filter by state.
	deadRows, err := store.List(ctx, jobs.Filter{State: "dead"}, 50, 0)
	require.NoError(t, err)
	require.Len(t, deadRows, 1)
	require.Equal(t, dead, deadRows[0].ID)
	require.True(t, deadRows[0].LastError.Valid)
	require.Equal(t, "kubo pin timeout", deadRows[0].LastError.String, "last_error surfaced for the jobs view")
	require.EqualValues(t, 5, deadRows[0].Attempts)
	dn, err := store.Count(ctx, jobs.Filter{State: "dead"})
	require.NoError(t, err)
	require.EqualValues(t, 1, dn)

	// Filter by kind.
	wh, err := store.List(ctx, jobs.Filter{Kind: "webhook_emit"}, 50, 0)
	require.NoError(t, err)
	require.Len(t, wh, 1)
	require.Equal(t, oldest, wh[0].ID)

	// A pending derivative_prewarm carries no last_error.
	pending, err := store.List(ctx, jobs.Filter{State: "pending", Kind: "derivative_prewarm"}, 50, 0)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.False(t, pending[0].LastError.Valid)

	// Pagination: two pages of 2, no overlap.
	page1, err := store.List(ctx, jobs.Filter{}, 2, 0)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	page2, err := store.List(ctx, jobs.Filter{}, 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	require.NotEqual(t, page1[0].ID, page2[0].ID)
	require.NotEqual(t, page1[1].ID, page2[0].ID)

	// An unrecognized state matches nothing (handler validates → 400 before here).
	none, err := store.List(ctx, jobs.Filter{State: "bogus"}, 50, 0)
	require.NoError(t, err)
	require.Empty(t, none)
}
