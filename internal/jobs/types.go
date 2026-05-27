package jobs

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Job is the leased work-item view returned by Queue.Lease.
type Job struct {
	ID          uuid.UUID
	Kind        string
	Payload     []byte
	Attempts    int
	MaxAttempts int
	CreatedAt   time.Time
}

// Handler is the user-supplied function that processes a leased job.
// Returning nil marks the job completed; returning an error transitions
// it to pending (retryable, if attempts < max_attempts) or dead.
type Handler func(ctx context.Context, payload []byte) error

// State is the lifecycle state of a row in the jobs table.
type State string

const (
	StatePending   State = "pending"
	StateLeased    State = "leased"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateDead      State = "dead"
)

// EnqueueOpt mutates the row inserted by Queue.Enqueue. Currently only
// MaxAttempts and NotBefore are exposed.
type EnqueueOpt func(*enqueueParams)

type enqueueParams struct {
	maxAttempts int
	notBefore   time.Time
}

// WithMaxAttempts overrides the default max_attempts (5) for a specific
// job. Use higher counts for idempotent kinds; lower counts for kinds
// where failure indicates a non-transient problem.
func WithMaxAttempts(n int) EnqueueOpt {
	return func(p *enqueueParams) { p.maxAttempts = n }
}

// WithNotBefore schedules the job for later execution. Pass time.Now()
// for "asap" (the default behavior).
func WithNotBefore(t time.Time) EnqueueOpt {
	return func(p *enqueueParams) { p.notBefore = t }
}
