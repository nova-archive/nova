package lifecycle

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nova-archive/nova/internal/db/gen"
)

// sweepBatch bounds the rows claimed per tick so a large backlog drains over
// several ticks rather than in one long transaction.
const sweepBatch = 256

// Sweeper tombstones overdue soft-deletes on a fixed interval — the owner-
// deletion analogue of the M9 moderation scheduled-tombstone sweep. It re-reads
// the overdue claim each tick (no persisted in-flight state), so a restore (which
// clears soft_deleted_at) simply takes a row out of scope. It is deliberately
// decoupled from the write-heavy jobs.Queue.
type Sweeper struct {
	svc      *Service
	interval time.Duration
	enabled  bool
	log      *slog.Logger
}

// NewSweeper builds a Sweeper over svc. A non-positive interval defaults to one
// minute. When enabled is false, Run is a no-op (the rest of the lifecycle — the
// owner soft-delete endpoint — still works without the sweep).
func NewSweeper(svc *Service, interval time.Duration, enabled bool, log *slog.Logger) *Sweeper {
	if interval <= 0 {
		interval = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{svc: svc, interval: interval, enabled: enabled, log: log}
}

// Run ticks every interval until ctx is cancelled, tombstoning overdue
// soft-deletes on each tick. A disabled sweeper returns immediately.
func (s *Sweeper) Run(ctx context.Context) {
	if !s.enabled {
		return
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick runs one sweep pass: claim overdue soft-deletes (older than now-grace,
// excluding legal-held trees) and tombstone each via the shared primitive. A
// per-row failure is logged and never stops the pass — the claim already filters
// legal holds, and the no_shred_under_legal_hold CHECK is the backstop. Exported
// so tests (and a future "run now" action) can drive a pass without wall-clock
// waiting.
func (s *Sweeper) Tick(ctx context.Context) {
	cutoff := pgtype.Timestamptz{Time: s.svc.now().Add(-s.svc.grace), Valid: true}
	cids, err := s.svc.q.ListOverdueSoftDeletes(ctx, gen.ListOverdueSoftDeletesParams{Cutoff: cutoff, Lim: sweepBatch})
	if err != nil {
		s.log.Warn("lifecycle: sweep list overdue", "err", err)
		return
	}
	for _, cidStr := range cids {
		if err := s.svc.tombstoneOverdue(ctx, cidStr); err != nil {
			s.log.Warn("lifecycle: sweep tombstone", "cid", cidStr, "err", err)
		}
	}
}
