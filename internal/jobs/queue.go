package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Queue is a Postgres-backed FIFO job queue. The queue does not pool
// across processes — there is one Queue per coordinator process —
// but it is safe to share across goroutines within a process.
type Queue struct {
	pool *pgxpool.Pool
}

// NewQueue returns a Queue backed by the given pool.
func NewQueue(pool *pgxpool.Pool) *Queue {
	return &Queue{pool: pool}
}

// Enqueue inserts a pending row and returns the new job id (string form
// of the row's UUID).
//
// Payload MUST be valid UTF-8 (typically JSON). It is stored in the
// jsonb payload column via to_jsonb(text), which rejects non-UTF-8
// byte sequences with a Postgres encoding error. Job kinds carry
// metadata (CIDs, sizes, preset names), not raw blob bytes — encode
// any binary fields (e.g. base64) before enqueuing.
//
// Default max_attempts is 5; override via WithMaxAttempts.
// Default not_before is now(); override via WithNotBefore.
func (q *Queue) Enqueue(ctx context.Context, kind string, payload []byte, opts ...EnqueueOpt) (string, error) {
	if kind == "" {
		return "", errors.New("jobs: enqueue: kind is required")
	}
	if payload == nil {
		payload = []byte(`{}`)
	}

	p := enqueueParams{maxAttempts: 5, notBefore: time.Now()}
	for _, o := range opts {
		o(&p)
	}

	// Wrap payload as a JSON string so we get byte-faithful round-trip
	// regardless of whether the caller's bytes are themselves valid
	// JSON. Postgres normalizes jsonb (whitespace, key order), which
	// would otherwise break the bytes-in/bytes-out contract that
	// handlers depend on. Unwrap via (payload #>> '{}') on read.
	var id uuid.UUID
	err := q.pool.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, max_attempts, not_before)
		VALUES ($1, to_jsonb($2::text), $3, $4)
		RETURNING id
	`, kind, string(payload), p.maxAttempts, p.notBefore).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("jobs: enqueue: %w", err)
	}
	return id.String(), nil
}

// Lease atomically claims one pending job whose not_before is in the
// past. Returns ErrNoJobsAvailable if no row matches. Sets lease_until
// to now() + leaseDuration; the worker MUST complete or fail before
// the lease expires, otherwise the reclaim ticker will return the row
// to pending.
//
// Lease uses SELECT … FOR UPDATE SKIP LOCKED so concurrent leasers
// each receive distinct rows without coordination.
func (q *Queue) Lease(ctx context.Context, leaseDuration time.Duration) (*Job, error) {
	tx, err := q.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("jobs: lease: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id        uuid.UUID
		kind      string
		payload   []byte
		attempts  int
		maxAttn   int
		createdAt time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT id, kind, (payload #>> '{}')::bytea, attempts, max_attempts, created_at
		FROM jobs
		WHERE state = 'pending' AND not_before <= now()
		ORDER BY created_at ASC, id ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&id, &kind, &payload, &attempts, &maxAttn, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoJobsAvailable
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: lease: select: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE jobs
		   SET state = 'leased', lease_until = now() + ($2 || ' seconds')::interval
		 WHERE id = $1 AND created_at = $3
	`, id, fmt.Sprintf("%d", int(leaseDuration.Seconds())), createdAt)
	if err != nil {
		return nil, fmt.Errorf("jobs: lease: update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("jobs: lease: commit: %w", err)
	}

	return &Job{
		ID:          id,
		Kind:        kind,
		Payload:     payload,
		Attempts:    attempts,
		MaxAttempts: maxAttn,
		CreatedAt:   createdAt,
	}, nil
}

// Complete transitions a leased job to 'completed'. No-op if the row
// is already in a terminal state.
func (q *Queue) Complete(ctx context.Context, jobID string) error {
	id, err := uuid.Parse(jobID)
	if err != nil {
		return fmt.Errorf("jobs: complete: bad uuid: %w", err)
	}
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs
		   SET state = 'completed', lease_until = NULL
		 WHERE id = $1 AND state = 'leased'
	`, id)
	if err != nil {
		return fmt.Errorf("jobs: complete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotFound
	}
	return nil
}

// Fail records a handler error. If attempts+1 < max_attempts the row
// returns to 'pending' with not_before set per Backoff(attempts+1).
// Otherwise the row transitions to 'dead' for operator inspection.
func (q *Queue) Fail(ctx context.Context, jobID string, cause error) error {
	id, err := uuid.Parse(jobID)
	if err != nil {
		return fmt.Errorf("jobs: fail: bad uuid: %w", err)
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	tx, err := q.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("jobs: fail: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var attempts, maxAttempts int
	err = tx.QueryRow(ctx, `
		SELECT attempts, max_attempts FROM jobs WHERE id = $1 AND state = 'leased'
	`, id).Scan(&attempts, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrJobNotFound
	}
	if err != nil {
		return fmt.Errorf("jobs: fail: select: %w", err)
	}
	attempts++

	if attempts >= maxAttempts {
		_, err = tx.Exec(ctx, `
			UPDATE jobs
			   SET state = 'dead', attempts = $2, last_error = $3, lease_until = NULL
			 WHERE id = $1
		`, id, attempts, msg)
	} else {
		delay := Backoff(attempts)
		_, err = tx.Exec(ctx, `
			UPDATE jobs
			   SET state = 'pending', attempts = $2, last_error = $3,
			       lease_until = NULL,
			       not_before = now() + ($4 || ' seconds')::interval
			 WHERE id = $1
		`, id, attempts, msg, fmt.Sprintf("%d", int(delay.Seconds())))
	}
	if err != nil {
		return fmt.Errorf("jobs: fail: update: %w", err)
	}
	return tx.Commit(ctx)
}

// ReclaimExpiredLeases returns rows whose lease has expired back to
// 'pending'. Called by WorkerPool.Run on a 10-second ticker.
func (q *Queue) ReclaimExpiredLeases(ctx context.Context) (int, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs
		   SET state = 'pending', lease_until = NULL
		 WHERE state = 'leased' AND lease_until < now()
	`)
	if err != nil {
		return 0, fmt.Errorf("jobs: reclaim: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
