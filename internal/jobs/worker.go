package jobs

import (
	"context"
	"errors"
	"sync"
	"time"
)

// WorkerOptions tunes a WorkerPool. Defaults: 4 concurrent workers,
// 250ms poll interval, 30s lease duration. Choose lease duration so
// the slowest handler comfortably finishes within it; on lease expiry
// another worker re-leases and runs the handler again (handlers MUST
// be idempotent).
type WorkerOptions struct {
	Concurrency   int
	PollInterval  time.Duration
	LeaseDuration time.Duration
}

// WorkerPool consumes jobs from a Queue and dispatches them to
// registered handlers. Register all handlers BEFORE calling Run; the
// pool does not support adding handlers after Run starts.
type WorkerPool struct {
	q        *Queue
	opts     WorkerOptions
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewWorkerPool returns a pool over q with the given options. Defaults
// are applied to zero-valued option fields.
func NewWorkerPool(q *Queue, opts WorkerOptions) *WorkerPool {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 250 * time.Millisecond
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = 30 * time.Second
	}
	return &WorkerPool{
		q:        q,
		opts:     opts,
		handlers: make(map[string]Handler),
	}
}

// RegisterHandler associates kind with h. Re-registration overwrites.
// Call before Run.
func (wp *WorkerPool) RegisterHandler(kind string, h Handler) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	wp.handlers[kind] = h
}

// Run blocks until ctx is cancelled. It spawns Concurrency leaser
// goroutines plus one reclaim ticker. Each leaser polls Lease at
// PollInterval; when Lease returns a job, the leaser dispatches to
// the registered handler.
//
// Run is safe to invoke once per WorkerPool. To restart a stopped
// pool, construct a new WorkerPool.
func (wp *WorkerPool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < wp.opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wp.leaserLoop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		wp.reclaimLoop(ctx)
	}()
	wg.Wait()
}

func (wp *WorkerPool) leaserLoop(ctx context.Context) {
	t := time.NewTicker(wp.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			wp.tryOne(ctx)
		}
	}
}

func (wp *WorkerPool) tryOne(ctx context.Context) {
	job, err := wp.q.Lease(ctx, wp.opts.LeaseDuration)
	if errors.Is(err, ErrNoJobsAvailable) {
		return
	}
	if err != nil {
		// Lease errors are transient (DB blips); log and continue. We
		// don't have a structured logger plumbed in M2; later milestones
		// inject one via WorkerOptions.
		return
	}

	wp.mu.RLock()
	h, ok := wp.handlers[job.Kind]
	wp.mu.RUnlock()

	if !ok {
		_ = wp.q.Fail(ctx, job.ID.String(), ErrUnknownKind)
		return
	}

	if err := h(ctx, job.Payload); err != nil {
		_ = wp.q.Fail(ctx, job.ID.String(), err)
		return
	}
	_ = wp.q.Complete(ctx, job.ID.String())
}

func (wp *WorkerPool) reclaimLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = wp.q.ReclaimExpiredLeases(ctx)
		}
	}
}
