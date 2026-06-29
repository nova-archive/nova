package orchestrator

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/notify"
)

// LivenessConfig carries the FED v2 liveness timers (from config.Federation). The
// suspect threshold is SuspectAfterMissed × HeartbeatInterval; unreachable and
// evicted are absolute durations from the last evidence of life.
type LivenessConfig struct {
	HeartbeatInterval  time.Duration
	SuspectAfterMissed int
	UnreachableAfter   time.Duration
	EvictedAfter       time.Duration
}

func (c LivenessConfig) suspectSeconds() int32 {
	return int32(c.SuspectAfterMissed) * int32(c.HeartbeatInterval.Seconds())
}

// SweepResult tallies a sweep's transitions for observability.
type SweepResult struct {
	ToSuspect, ToUnreachable, ToEvicted, Revoked int
}

// livenessRank orders statuses by severity so the sweeper only ever advances a
// node toward failure (reactivation is the heartbeat handler's job, D-M5-4a).
func livenessRank(s gen.NodeStatus) int {
	switch s {
	case gen.NodeStatusActive:
		return 0
	case gen.NodeStatusSuspect:
		return 1
	case gen.NodeStatusUnreachable:
		return 2
	case gen.NodeStatusEvicted:
		return 3
	case gen.NodeStatusRevoked:
		return 4
	}
	return -1
}

// ReconcileNodeLiveness runs one liveness sweep (D-M5-4/4-REVOKE/-OBS): it advances
// each silent node to the status its silence warrants and applies the transition
// fallout — projection enqueue/dirty for affected CIDs, failing the node's
// still-pending reservations, deleting an evicted node's assignments — then signals
// newly-revoked nodes exactly once. Each node's fallout commits in its own bounded
// transaction (D-M5-2d); recompute of the enqueued CIDs is the async drain's job
// (Task 2). Notifier emission is best-effort and happens AFTER commit.
func ReconcileNodeLiveness(ctx context.Context, pool *pgxpool.Pool, cfg LivenessConfig, n notify.Notifier) (SweepResult, error) {
	var res SweepResult
	q := gen.New(pool)

	rows, err := q.SelectLivenessTransitions(ctx, gen.SelectLivenessTransitionsParams{
		EvictedSecs:     int32(cfg.EvictedAfter.Seconds()),
		UnreachableSecs: int32(cfg.UnreachableAfter.Seconds()),
		SuspectSecs:     cfg.suspectSeconds(),
	})
	if err != nil {
		return res, err
	}
	for _, row := range rows {
		if livenessRank(row.TargetStatus) <= livenessRank(row.Status) {
			continue // computed target is not a strict advancement; skip
		}
		if err := applyTransition(ctx, pool, row.ID, row.TargetStatus); err != nil {
			return res, err
		}
		switch row.TargetStatus {
		case gen.NodeStatusSuspect:
			res.ToSuspect++
		case gen.NodeStatusUnreachable:
			res.ToUnreachable++
		case gen.NodeStatusEvicted:
			res.ToEvicted++
		}
	}

	// Revocation observation (D-M5-4-REVOKE-OBS): novactl revoke is DB-direct, so the
	// sweeper is the coordinator-local path that emits federation.node_revoked once.
	revoked, err := q.SelectUnsignaledRevoked(ctx)
	if err != nil {
		return res, err
	}
	for _, id := range revoked {
		if err := applyRevokeFallout(ctx, pool, id, n); err != nil {
			return res, err
		}
		res.Revoked++
	}
	return res, nil
}

// applyTransition commits one node's status change plus its fallout in a single
// bounded transaction. Suspect still counts toward durability and stays a valid
// source, so it has no projection fallout. Unreachable/evicted drop countability,
// so they enqueue affected CIDs and free dead pending reservations; evicted
// additionally retires the rows (D-M5-4a-EVICT), AFTER the enqueue captured them.
func applyTransition(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID, target gen.NodeStatus) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	switch target {
	case gen.NodeStatusUnreachable:
		if err := enqueueNodeCIDs(ctx, q, id, "node_unreachable"); err != nil {
			return err
		}
		if _, err := q.FailNodePendingAssignments(ctx, id); err != nil {
			return err
		}
	case gen.NodeStatusEvicted:
		if err := enqueueNodeCIDs(ctx, q, id, "node_evicted"); err != nil {
			return err
		}
		if _, err := q.FailNodePendingAssignments(ctx, id); err != nil {
			return err
		}
	}

	if err := q.SetNodeStatus(ctx, gen.SetNodeStatusParams{ID: id, Status: target}); err != nil {
		return err
	}
	if target == gen.NodeStatusEvicted {
		if _, err := q.DeleteNodeAssignments(ctx, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// applyRevokeFallout handles a newly-revoked node: rows are RETAINED as forensic
// evidence (D-M5-4-REVOKE) — the recompute filter already excludes non-active/
// suspect, so enqueueing the affected CIDs is enough to drop them from durability.
// The event is emitted after commit, and revoked_signaled_at gates emit-once.
func applyRevokeFallout(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID, n notify.Notifier) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)
	if err := enqueueNodeCIDs(ctx, q, id, "node_revoked"); err != nil {
		return err
	}
	if err := q.MarkRevokedSignaled(ctx, id); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	n.Emit(ctx, notify.Event{
		Type:     "federation.node_revoked",
		ScopeKey: uuid.UUID(id.Bytes).String(),
	})
	return nil
}

// enqueueNodeCIDs marks the node's CIDs dirty and durably queues them for bounded
// async recompute (the two halves of D-M5-2d's bulk-transition contract).
func enqueueNodeCIDs(ctx context.Context, q *gen.Queries, id pgtype.UUID, reason string) error {
	if err := q.MarkReplicationDirtyForNode(ctx, id); err != nil {
		return err
	}
	return q.EnqueueReconcileForNode(ctx, gen.EnqueueReconcileForNodeParams{Reason: reason, NodeID: id})
}
