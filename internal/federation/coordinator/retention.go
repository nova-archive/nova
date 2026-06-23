package coordinator

import (
	"context"
	"log/slog"
	"time"
)

// pruneOnce deletes change-log rows older than retention and advances the
// watermark (one atomic statement). A donor whose cursor predates the watermark
// then recovers via snapshot. NB: sqlc maps timestamptz→time.Time (see
// internal/db/sqlc.yaml override), so PruneChangeLog takes a time.Time directly.
func (s *Server) pruneOnce(ctx context.Context, retention time.Duration) error {
	wm, err := s.q.PruneChangeLog(ctx, time.Now().Add(-retention))
	if err != nil {
		return err
	}
	slog.Info("fed.changelog.pruned", "pruned_through_seq", wm)
	return nil
}

// runRetention prunes on a ticker until ctx is cancelled. Started from Run.
func (s *Server) runRetention(ctx context.Context, interval, retention time.Duration) {
	if interval <= 0 || retention <= 0 {
		return // retention disabled
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.pruneOnce(ctx, retention); err != nil {
				slog.Warn("fed.changelog.prune_failed", "err", err)
			}
		}
	}
}
