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
}

// MetricsConfig enables the per-tick concentration + attrition signals (D-M5-10/11).
type MetricsConfig struct {
	TopK          int
	Concentration ConcentrationThresholds
	Attrition     AttritionConfig
}

// SetMetrics enables the concentration + slow-attrition signals on each tick.
func (o *Orchestrator) SetMetrics(m MetricsConfig) { o.metrics = &m }

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
	healed, err := o.scheduler.Tick(ctx)
	if err != nil {
		slog.Warn("orchestrator.tick.error", "err", err)
	} else if healed > 0 {
		slog.Info("orchestrator.heal.tick", "scheduled", healed)
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
