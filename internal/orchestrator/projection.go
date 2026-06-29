// Package orchestrator implements P2-M5 liveness & healing: the 5-state liveness
// sweeper, the healing tick loop, and the blob_replication_state projection
// (donor-replica health). The projection is rebuildable cache; authority remains
// pin_assignments ⨝ node liveness.
package orchestrator

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
)

// ReplicationTargets is the per-content-class replication factor R (from
// orchestrator.replication.factor). The orchestrator owns it so the query layer
// stays config-free.
type ReplicationTargets struct {
	Important int
	Normal    int
	Cache     int
}

// For returns R for a durability class; unknown classes fall back to Normal (the
// CHECK constraint makes other values impossible in practice).
func (t ReplicationTargets) For(class string) int {
	switch class {
	case "important":
		return t.Important
	case "cache":
		return t.Cache
	default:
		return t.Normal
	}
}

// safetyTier classifies a CID from its acked-on-countable-nodes count against the
// class target (D-M5-2c): 0 ⇒ donor_lost (no donor holder; may still be
// local_recoverable), 1 ⇒ tier1 (one failure from loss), 2..target-1 ⇒ tier2
// (non-compliant but safe), ≥target ⇒ healthy. The ≥target check precedes the
// tier1 check so target=1 yields healthy, not tier1, at one copy.
func safetyTier(healthy, target int) string {
	switch {
	case healthy == 0:
		return "donor_lost"
	case healthy >= target:
		return "healthy"
	case healthy == 1:
		return "tier1"
	default:
		return "tier2"
	}
}

// RecomputeCID recomputes one CID's projection row from authority and upserts it,
// under a per-CID advisory lock so admission, healing, ack/fail, and the reconcile
// drain cannot compute from or write stale counts concurrently (D-M5-2d). The lock
// is orthogonal to AssignPin's global change-log lock. The caller passes an open
// transaction; the lock and the upsert share its scope. A CID with no
// blob_storage_state row (not yet tracked) is a no-op.
func RecomputeCID(ctx context.Context, tx pgx.Tx, cid string, targets ReplicationTargets) error {
	q := gen.New(tx)
	if err := q.LockReplicationCID(ctx, cid); err != nil {
		return err
	}
	class, err := q.GetReplicationDurabilityClass(ctx, cid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // untracked blob; nothing to project
		}
		return err
	}
	counts, err := q.RecomputeReplicationCounts(ctx, cid)
	if err != nil {
		return err
	}
	local, err := q.GetLocalRecoverable(ctx, cid)
	if err != nil {
		return err
	}
	target := targets.For(class)
	return q.UpsertReplicationState(ctx, gen.UpsertReplicationStateParams{
		Cid:                  cid,
		HealthyAckedCount:    counts.HealthyAcked,
		SourceableAckedCount: counts.SourceableAcked,
		InFlightCount:        counts.InFlight,
		TargetCount:          int32(target),
		SafetyTier:           safetyTier(int(counts.HealthyAcked), target),
		LocalRecoverable:     local,
		DurabilityClass:      class,
	})
}

// DrainReconcile recomputes up to `batch` queued CIDs from authority — each in its
// own short transaction (recompute + dequeue atomically) — and returns the number
// processed. Bounded and idempotent: a provider-purge that dirties a huge set is
// drained in batches, never one unbounded transaction (D-M5-2d).
func DrainReconcile(ctx context.Context, pool *pgxpool.Pool, batch int, targets ReplicationTargets) (int, error) {
	cids, err := gen.New(pool).ListReconcileBatch(ctx, int32(batch))
	if err != nil {
		return 0, err
	}
	done := 0
	for _, cid := range cids {
		if err := reconcileOne(ctx, pool, cid, targets); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

func reconcileOne(ctx context.Context, pool *pgxpool.Pool, cid string, targets ReplicationTargets) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := RecomputeCID(ctx, tx, cid, targets); err != nil {
		return err
	}
	if err := gen.New(tx).DeleteReconciled(ctx, cid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecomputeTargets resets target_count for a class after an R change and enqueues
// the class's CIDs so the drain recomputes their safety_tier (D-M5-2b).
func RecomputeTargets(ctx context.Context, pool *pgxpool.Pool, class string, target int) error {
	q := gen.New(pool)
	if err := q.RecomputeTargetsForClass(ctx, gen.RecomputeTargetsForClassParams{
		DurabilityClass: class,
		TargetCount:     int32(target),
	}); err != nil {
		return err
	}
	return q.EnqueueReconcileByClass(ctx, class)
}
