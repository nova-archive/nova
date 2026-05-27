package jobs_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/jobs"
	"github.com/stretchr/testify/require"
)

func TestIntegrationWorkerProcessesEnqueuedJob(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	var calls atomic.Int32
	done := make(chan struct{})
	wp := jobs.NewWorkerPool(q, jobs.WorkerOptions{
		Concurrency:   2,
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 30 * time.Second,
	})
	wp.RegisterHandler("test.count", func(ctx context.Context, payload []byte) error {
		if calls.Add(1) == 3 {
			close(done)
		}
		return nil
	})

	for i := 0; i < 3; i++ {
		_, err := q.Enqueue(ctx, "test.count", nil)
		require.NoError(t, err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	go wp.Run(runCtx)

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not process 3 jobs within 15s")
	}
	runCancel()
}

func TestIntegrationWorkerRetriesRetryableFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	var attempts atomic.Int32
	done := make(chan struct{})

	wp := jobs.NewWorkerPool(q, jobs.WorkerOptions{
		Concurrency:   1,
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 30 * time.Second,
	})
	wp.RegisterHandler("test.fail-twice", func(ctx context.Context, payload []byte) error {
		n := attempts.Add(1)
		if n < 3 {
			return errors.New("flaky")
		}
		close(done)
		return nil
	})

	_, err := q.Enqueue(ctx, "test.fail-twice", nil, jobs.WithMaxAttempts(5))
	require.NoError(t, err)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go wp.Run(runCtx)

	// Worker uses Backoff() so retries wait 5s, 10s, ... To make the
	// test fast, advance not_before manually after each failure.
	go func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(200 * time.Millisecond):
				_, _ = pool.Exec(runCtx,
					`UPDATE jobs SET not_before = now() WHERE state = 'pending' AND not_before > now()`)
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestIntegrationWorkerHandlesUnknownKindAsDead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)
	wp := jobs.NewWorkerPool(q, jobs.WorkerOptions{
		Concurrency:   1,
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 5 * time.Second,
	})
	// Intentionally no handler registered for "test.unknown".

	id, err := q.Enqueue(ctx, "test.unknown", nil, jobs.WithMaxAttempts(1))
	require.NoError(t, err)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go wp.Run(runCtx)

	require.Eventually(t, func() bool {
		var state string
		_ = pool.QueryRow(runCtx, `SELECT state::text FROM jobs WHERE id = $1`, id).Scan(&state)
		return state == "dead"
	}, 10*time.Second, 100*time.Millisecond)
}

func TestIntegrationWorkerStopsOnContextCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)
	wp := jobs.NewWorkerPool(q, jobs.WorkerOptions{
		Concurrency:   1,
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 5 * time.Second,
	})

	runCtx, runCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		wp.Run(runCtx)
	}()
	runCancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop within 5s of context cancel")
	}
}
