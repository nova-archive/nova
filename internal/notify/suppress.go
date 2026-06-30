package notify

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
)

// Suppressor gates a scoped event so the same (event_type, destination, scope_key)
// fires at most once per window (D-M5-9a). A nil Suppressor fires every event
// (unsuppressed deployments / tests that don't exercise suppression).
type Suppressor interface {
	TryFire(ctx context.Context, eventType, dest, scopeKey string, windowSeconds int) (bool, error)
}

// DBSuppressor is the durable, restart-safe suppression store over the
// webhook_suppression table.
type DBSuppressor struct{ q *gen.Queries }

// NewDBSuppressor builds a suppression store backed by pool.
func NewDBSuppressor(pool *pgxpool.Pool) *DBSuppressor { return &DBSuppressor{q: gen.New(pool)} }

// TryFire atomically records and reports whether the scoped event may fire now.
// windowSeconds=0 always fires.
func (s *DBSuppressor) TryFire(ctx context.Context, eventType, dest, scopeKey string, windowSeconds int) (bool, error) {
	_, err := s.q.TryFireSuppression(ctx, gen.TryFireSuppressionParams{
		EventType: eventType, Destination: dest, ScopeKey: scopeKey, WindowSeconds: int32(windowSeconds),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // still inside the window ⇒ suppressed
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

var _ Suppressor = (*DBSuppressor)(nil)
