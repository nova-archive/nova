package moderation

import (
	"context"
	"log/slog"
	"time"
)

// Sweeper tombstones overdue quarantines on a fixed interval — the in-process
// analogue of the DMCA procedure's "scheduled job [that] runs every minute". It
// re-reads moderation_decisions each tick (no persisted in-flight state), so
// counter-notice and restore — which clear scheduled_tombstone_at — simply take
// a row out of scope. It is deliberately decoupled from the write-heavy
// jobs.Queue.
type Sweeper struct {
	svc      *Service
	interval time.Duration
	enabled  bool
	log      *slog.Logger
}

// NewSweeper builds a Sweeper over svc. A non-positive interval defaults to one
// minute. When enabled is false, Run is a no-op (the rest of moderation — the
// admin API, intake, blocklist — still works without the sweep).
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
// quarantines on each tick. A disabled sweeper returns immediately.
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

// Tick runs one sweep pass, tombstoning every overdue, still-quarantined,
// non-legal-hold quarantine. It is exported so tests (and a future "run now"
// admin/CLI action) can drive a pass without waiting on wall-clock cadence. A
// per-row failure is logged and never stops the pass: the listing already
// filters out legal-hold rows, and the no_shred_under_legal_hold CHECK is the
// backstop if one ever slips through.
func (s *Sweeper) Tick(ctx context.Context) {
	rows, err := s.svc.q.ListOverdueTombstones(ctx)
	if err != nil {
		s.log.Warn("moderation: sweep list overdue", "err", err)
		return
	}
	for _, r := range rows {
		ruleRef := ""
		if r.RuleRef.Valid {
			ruleRef = r.RuleRef.String
		}
		if err := s.svc.Tombstone(ctx, TombstoneCmd{
			CID:     r.Cid,
			Rule:    r.Rule,
			RuleRef: ruleRef,
			Reason:  "scheduled tombstone",
		}); err != nil {
			s.log.Warn("moderation: sweep tombstone", "cid", r.Cid, "err", err)
		}
	}
}
