package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/notify"
)

// Orchestrator runs the periodic single-leader healing loop (D-M5-6): each tick
// sweeps node liveness, then runs a healing tick (which itself drains the reconcile
// queue). It owns no durable state; a context cancel stops it and a restart
// re-derives all work from the projection. cmd/coordinator starts it after the DB
// and federation listener are up.
type Orchestrator struct {
	pool         *pgxpool.Pool
	liveness     LivenessConfig
	scheduler    *Scheduler
	notifier     notify.Notifier
	tickInterval time.Duration
	metrics      *MetricsConfig
	belowFloor   *BelowFloorConfig
}

// MetricsConfig enables the per-tick concentration + attrition signals (D-M5-10/11).
type MetricsConfig struct {
	TopK          int
	Concentration ConcentrationThresholds
	Attrition     AttritionConfig
}

// SetMetrics enables the concentration + slow-attrition signals on each tick.
func (o *Orchestrator) SetMetrics(m MetricsConfig) { o.metrics = &m }

// SetBelowFloor enables the D-M7.1-3 below-floor replacement sweep on each tick:
// marker maintenance before the healing tick, requeue + replace-then-demote
// after it. Without this the below_floor_since marker is never set and the
// Task-10 count exclusion is inert.
func (o *Orchestrator) SetBelowFloor(c BelowFloorConfig) { o.belowFloor = &c }

// NewOrchestrator wires the loop. A non-positive tickInterval defaults to 60s.
func NewOrchestrator(pool *pgxpool.Pool, liveness LivenessConfig, scheduler *Scheduler, n notify.Notifier, tickInterval time.Duration) *Orchestrator {
	if tickInterval <= 0 {
		tickInterval = 60 * time.Second
	}
	if n == nil {
		n = notify.NoopNotifier{}
	}
	return &Orchestrator{pool: pool, liveness: liveness, scheduler: scheduler, notifier: n, tickInterval: tickInterval}
}

// Run blocks until ctx is cancelled, running one pass immediately and then on
// every tick.
func (o *Orchestrator) Run(ctx context.Context) {
	t := time.NewTicker(o.tickInterval)
	defer t.Stop()
	o.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.runOnce(ctx)
		}
	}
}

func (o *Orchestrator) runOnce(ctx context.Context) {
	sweep, err := ReconcileNodeLiveness(ctx, o.pool, o.liveness, o.notifier)
	if err != nil {
		slog.Warn("orchestrator.liveness.error", "err", err)
	} else if sweep.ToSuspect+sweep.ToUnreachable+sweep.ToEvicted+sweep.Revoked > 0 {
		slog.Info("orchestrator.liveness.tick", "suspect", sweep.ToSuspect, "unreachable", sweep.ToUnreachable,
			"evicted", sweep.ToEvicted, "revoked", sweep.Revoked)
	}
	// Below-floor marker maintenance runs BEFORE the healing tick so the
	// projection recomputes against fresh markers; it always runs (observability)
	// regardless of whether the remedy is enabled.
	if o.belowFloor != nil {
		if mr, err := MaintainBelowFloorMarkers(ctx, o.pool, *o.belowFloor); err != nil {
			slog.Warn("orchestrator.below_floor.markers.error", "err", err)
		} else if mr.Marked+mr.Cleared > 0 {
			slog.Info("orchestrator.below_floor.markers", "marked", mr.Marked, "cleared", mr.Cleared)
		}
	}
	healed, err := o.scheduler.Tick(ctx)
	if err != nil {
		slog.Warn("orchestrator.tick.error", "err", err)
	} else if healed > 0 {
		slog.Info("orchestrator.heal.tick", "scheduled", healed)
	}
	// The below-floor remedy runs AFTER the tick's donor_lost + tier1 passes so a
	// fresh emergency always outranks replacing a replica that still exists.
	if o.belowFloor != nil && o.belowFloor.Enabled {
		if rr, err := ReplaceBelowFloor(ctx, o.pool, *o.belowFloor); err != nil {
			slog.Warn("orchestrator.below_floor.remedy.error", "err", err)
		} else if rr.Requeued+rr.Demoted > 0 {
			slog.Info("orchestrator.below_floor.remedy", "requeued", rr.Requeued, "demoted", rr.Demoted)
		}
	}

	if o.metrics != nil {
		if err := EmitConcentration(ctx, o.pool, o.notifier, o.metrics.TopK, o.metrics.Concentration); err != nil {
			slog.Warn("orchestrator.concentration.error", "err", err)
		}
		if err := EvaluateAttrition(ctx, o.pool, o.notifier, o.metrics.Attrition); err != nil {
			slog.Warn("orchestrator.attrition.error", "err", err)
		}
	}
}
