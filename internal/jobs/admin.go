package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AdminStore is the read-only introspection surface over the jobs table for the
// M11 admin jobs view. It is deliberately separate from Queue (no mutation — the
// jobs view shows stuck / failed / recent work and last_error, with retry a
// later additive fast-follow) and is served by jobs_state_kind_idx
// (state, kind, created_at DESC).
type AdminStore struct {
	pool *pgxpool.Pool
}

// NewAdminStore returns an AdminStore backed by the given pool.
func NewAdminStore(pool *pgxpool.Pool) *AdminStore { return &AdminStore{pool: pool} }

// JobRow is the admin projection of a jobs row — richer than the leased Job
// view (carries state, last_error, lease timing). Nullable columns use pgtype so
// a NULL never crashes the scan; the handler maps them to its JSON DTO.
type JobRow struct {
	ID          uuid.UUID
	Kind        string
	State       string
	Attempts    int
	MaxAttempts int
	LastError   pgtype.Text
	NotBefore   time.Time
	LeaseUntil  pgtype.Timestamptz
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Filter narrows a listing. An empty field means "no filter on that column".
type Filter struct {
	State string
	Kind  string
}

// List returns jobs newest-first, optionally filtered by state and/or kind. An
// unrecognized state simply matches nothing (the handler validates and returns
// 400 before reaching here); state is compared as text so a bad value never
// raises an enum-cast error.
func (s *AdminStore) List(ctx context.Context, f Filter, limit, offset int) ([]JobRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, state::text, attempts, max_attempts, last_error,
		       not_before, lease_until, created_at, updated_at
		FROM jobs
		WHERE ($1 = '' OR state::text = $1) AND ($2 = '' OR kind = $2)
		ORDER BY created_at DESC, id DESC
		LIMIT $3 OFFSET $4
	`, f.State, f.Kind, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("jobs: admin list: %w", err)
	}
	defer rows.Close()

	out := make([]JobRow, 0, limit)
	for rows.Next() {
		var j JobRow
		if err := rows.Scan(&j.ID, &j.Kind, &j.State, &j.Attempts, &j.MaxAttempts,
			&j.LastError, &j.NotBefore, &j.LeaseUntil, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, fmt.Errorf("jobs: admin list scan: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: admin list rows: %w", err)
	}
	return out, nil
}

// Count returns the total number of jobs matching the filter (for pagination).
func (s *AdminStore) Count(ctx context.Context, f Filter) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		WHERE ($1 = '' OR state::text = $1) AND ($2 = '' OR kind = $2)
	`, f.State, f.Kind).Scan(&n); err != nil {
		return 0, fmt.Errorf("jobs: admin count: %w", err)
	}
	return n, nil
}
