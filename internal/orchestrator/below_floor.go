package orchestrator

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
)

// BelowFloorConfig tunes the D-M7.1-3 below-floor replacement sweep: the
// distrust remedy for donors whose reputation has sunk below
// orchestrator.reputation_floor. It mirrors the drain treatment (D-M7-6) but
// for DISTRUSTED nodes instead of politely-leaving ones. The reputation floor
// itself lives in the scheduler; this block only tunes the REMEDY.
type BelowFloorConfig struct {
	// Enabled gates the requeue + replace-then-demote halves. Marker
	// maintenance runs regardless (observability — the nova_below_floor_nodes
	// gauge stays truthful with the remedy off).
	Enabled bool
	// ReputationFloor is the entry edge (score below it stamps the marker);
	// HysteresisMargin widens the exit edge (the marker clears only at score
	// >= floor + margin) so wobble near the floor never flaps counts.
	ReputationFloor  float64
	HysteresisMargin float64
	// GraceSeconds is the sustainment window: a marker younger than this still
	// counts toward safety and receives no replacement traffic.
	GraceSeconds float64
	// RequeueBatch caps both the reconcile enqueue and the demote per sweep,
	// across ALL sustained nodes, so below-floor churn never floods the queue.
	RequeueBatch int
	// OnRequeue, if non-nil, is invoked with the count of CIDs enqueued this
	// sweep — the process-local nova_below_floor_requeued_total observer seam.
	// Nil-safe so the orchestrator runs without a metrics registry (tests).
	OnRequeue func(int)
}

// BelowFloorMarkerResult tallies one marker-maintenance pass.
type BelowFloorMarkerResult struct {
	Marked, Cleared int
}

// BelowFloorRemedyResult tallies one remedy pass.
type BelowFloorRemedyResult struct {
	Requeued, Demoted int
}

// MaintainBelowFloorMarkers runs the hysteretic marker maintenance and ALWAYS
// runs (even with the remedy disabled) so the gauge stays truthful. It stamps
// below_floor_since on live nodes that have just sunk below the floor and clears
// it once a marked node's score recovers past floor + HysteresisMargin. It is
// ordered BEFORE the healing tick so the projection recomputes against fresh
// markers. Marking and clearing are two independent statements — a node cannot
// satisfy both edges in one pass (below floor vs at/above floor+margin), so
// order between them is immaterial.
func MaintainBelowFloorMarkers(ctx context.Context, pool *pgxpool.Pool, cfg BelowFloorConfig) (BelowFloorMarkerResult, error) {
	var res BelowFloorMarkerResult
	q := gen.New(pool)
	marked, err := q.MarkBelowFloorNodes(ctx, cfg.ReputationFloor)
	if err != nil {
		return res, err
	}
	res.Marked = int(marked)
	cleared, err := q.ClearBelowFloorNodes(ctx, cfg.ReputationFloor+cfg.HysteresisMargin)
	if err != nil {
		return res, err
	}
	res.Cleared = int(cleared)
	return res, nil
}

// ReplaceBelowFloor runs the remedy: bounded requeue of sustained-below-floor
// nodes' CIDs (their Task-10 count exclusion drops them below target, so they
// surface as tier1/tier2 and heal onto trusted nodes), then replace-then-demote
// — failing a sustained node's acked assignment ONLY once enough trusted
// holders exist without it. It is ordered AFTER the healing tick's donor_lost
// and tier1 passes: the enqueued CIDs are drained on subsequent ticks and walk
// the same tier priority, so a fresh tier1 emergency always outranks replacing
// a replica that still exists. Callers gate this on cfg.Enabled.
func ReplaceBelowFloor(ctx context.Context, pool *pgxpool.Pool, cfg BelowFloorConfig) (BelowFloorRemedyResult, error) {
	var res BelowFloorRemedyResult
	q := gen.New(pool)
	requeued, err := q.EnqueueBelowFloorReconcile(ctx, gen.EnqueueBelowFloorReconcileParams{
		GraceSecs:    cfg.GraceSeconds,
		RequeueBatch: int32(cfg.RequeueBatch),
	})
	if err != nil {
		return res, err
	}
	res.Requeued = int(requeued)
	if res.Requeued > 0 && cfg.OnRequeue != nil {
		cfg.OnRequeue(res.Requeued)
	}
	// Demote is safe-to-revoke-gated in SQL against AUTHORITY (not the possibly
	// stale-high projection), so a sole holder never demotes and no durability
	// dip is possible. Bounded by the same per-sweep cap.
	demoted, err := q.DemoteBelowFloorReplicas(ctx, gen.DemoteBelowFloorReplicasParams{
		GraceSecs:   cfg.GraceSeconds,
		DemoteBatch: int32(cfg.RequeueBatch),
	})
	if err != nil {
		return res, err
	}
	res.Demoted = int(demoted)
	return res, nil
}
