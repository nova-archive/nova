package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/jobs"
	"github.com/stretchr/testify/require"
)

func TestIntegrationQueueEnqueueAndLease(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	id, err := q.Enqueue(ctx, "test.echo", []byte(`{"msg":"hi"}`))
	require.NoError(t, err)
	require.NotEmpty(t, id)

	job, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, id, job.ID.String())
	require.Equal(t, "test.echo", job.Kind)
	require.Equal(t, []byte(`{"msg":"hi"}`), job.Payload)
	require.Equal(t, 0, job.Attempts)
}

func TestIntegrationQueueLeaseEmptyReturnsErr(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	_, err := q.Lease(ctx, 30*time.Second)
	require.ErrorIs(t, err, jobs.ErrNoJobsAvailable)
}

func TestIntegrationQueueLeaseSkipLocked(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	_, err := q.Enqueue(ctx, "test.echo", []byte("a"))
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, "test.echo", []byte("b"))
	require.NoError(t, err)

	j1, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)
	j2, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)
	require.NotEqual(t, j1.ID, j2.ID, "two concurrent leases must yield distinct jobs")

	_, err = q.Lease(ctx, 30*time.Second)
	require.ErrorIs(t, err, jobs.ErrNoJobsAvailable, "third lease finds nothing")
}

func TestIntegrationQueueCompleteRemovesFromLeased(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	_, err := q.Enqueue(ctx, "test.echo", []byte("c"))
	require.NoError(t, err)
	job, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, q.Complete(ctx, job.ID.String()))

	_, err = q.Lease(ctx, 30*time.Second)
	require.ErrorIs(t, err, jobs.ErrNoJobsAvailable)
}

func TestIntegrationQueueFailRetryableSchedulesBackoff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	_, err := q.Enqueue(ctx, "test.echo", []byte("d"))
	require.NoError(t, err)
	job, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)

	require.NoError(t, q.Fail(ctx, job.ID.String(), errors.New("transient")))

	// The row should be pending again, but not_before is in the future.
	_, err = q.Lease(ctx, 30*time.Second)
	require.ErrorIs(t, err, jobs.ErrNoJobsAvailable,
		"row is pending but not_before guards against immediate re-lease")
}

func TestIntegrationQueueFailExhaustsToDead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	id, err := q.Enqueue(ctx, "test.echo", []byte("e"), jobs.WithMaxAttempts(2))
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		// Push not_before to now so the next Lease succeeds.
		_, err = pool.Exec(ctx, `UPDATE jobs SET not_before = now() WHERE id = $1`, id)
		require.NoError(t, err)

		j, err := q.Lease(ctx, 30*time.Second)
		require.NoError(t, err)
		require.NoError(t, q.Fail(ctx, j.ID.String(), errors.New("still failing")))
	}

	// After max_attempts failures, the row is dead.
	var state string
	err = pool.QueryRow(ctx, `SELECT state::text FROM jobs WHERE id = $1`, id).Scan(&state)
	require.NoError(t, err)
	require.Equal(t, "dead", state)
}

func TestIntegrationQueueReclaimExpiredLeases(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	_, err := q.Enqueue(ctx, "test.echo", []byte("f"))
	require.NoError(t, err)
	_, err = q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)

	// Force the lease into the past.
	_, err = pool.Exec(ctx, `UPDATE jobs SET lease_until = now() - interval '1 minute' WHERE state = 'leased'`)
	require.NoError(t, err)

	n, err := q.ReclaimExpiredLeases(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Now Lease can pick it up again.
	j, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, j.ID)
}
