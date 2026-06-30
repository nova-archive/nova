package orchestrator

import (
	"context"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/notify"
)

// AttritionConfig tunes the mass-casualty + slow-attrition signals (D-M5-11).
type AttritionConfig struct {
	Targets                 ReplicationTargets
	MassCasualtyWindow      time.Duration
	MassCasualtyRatio       float64
	CapacityRunwayFloorDays float64
}

// EvaluateAttrition computes the mass-casualty + slow-attrition signals and emits
// federation.degraded / federation.shrinking via the Notifier (D-M5-11). It NEVER
// overrides any budget — these are operator signals only. The slow-attrition
// metrics are deliberately named for their true dimensions (days, headroom, trend);
// the old dimensionally-wrong "runway_days" is gone.
func EvaluateAttrition(ctx context.Context, pool *pgxpool.Pool, n notify.Notifier, cfg AttritionConfig) error {
	q := gen.New(pool)

	// Mass-casualty: an active→unreachable burst over the window, vs the active
	// population at window start (≈ surviving active + those just lost).
	recent, err := q.CountRecentlyUnreachable(ctx, int32(cfg.MassCasualtyWindow.Seconds()))
	if err != nil {
		return err
	}
	surv, err := q.SurvivingCapacity(ctx)
	if err != nil {
		return err
	}
	baseline := surv.ActiveCount + recent
	if baseline > 0 && cfg.MassCasualtyRatio > 0 && float64(recent) > cfg.MassCasualtyRatio*float64(baseline) {
		n.Emit(ctx, notify.Event{
			Type: "federation.degraded", ScopeKey: "global",
			Payload: map[string]any{
				"unreachable_in_window": recent,
				"baseline_active":       baseline,
				"window_seconds":        int(cfg.MassCasualtyWindow.Seconds()),
			},
		})
	}

	// Slow-attrition per class.
	corpus, err := q.SumCorpusBytesByClass(ctx)
	if err != nil {
		return err
	}
	for _, row := range corpus {
		r := cfg.Targets.For(row.DurabilityClass)
		desired := row.Bytes * int64(r) // desired_replicated_bytes = corpus_bytes_c × R_c
		if desired <= 0 {
			continue
		}
		// repair_time_days = desired_bytes / surviving_daily_egress_bytes  (bytes ÷
		// bytes/day = DAYS). With no surviving egress, the rebuild never completes.
		repairTimeDays := math.Inf(1)
		if surv.DailyEgress > 0 {
			repairTimeDays = float64(desired) / float64(surv.DailyEgress)
		}
		// storage_headroom = surviving_free_donor_bytes / projected_required_bytes.
		storageHeadroom := float64(surv.FreeBytes) / float64(desired)

		if repairTimeDays > cfg.CapacityRunwayFloorDays || storageHeadroom < 1.0 {
			n.Emit(ctx, notify.Event{
				Type: "federation.shrinking", ScopeKey: row.DurabilityClass,
				Payload: map[string]any{
					"limiting_class":    row.DurabilityClass,
					"repair_time_days":  repairTimeDays,
					"storage_headroom":  storageHeadroom,
					"active_node_trend": surv.ActiveCount,
				},
			})
		}
	}
	return nil
}
