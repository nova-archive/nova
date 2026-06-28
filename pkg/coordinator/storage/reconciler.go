package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/nova-archive/nova/internal/db/gen"
)

// reconcileBatchSize bounds one reconciler pass so a large staging backlog is
// drained over several ticks rather than in one long transaction-free sweep.
const reconcileBatchSize = 256

// Reconciler is the P2-M4.1 async durability reconciler. It periodically scans
// 'staging' blobs (written by the gate-on Put path) and, for each, either
//   - commits it (staging → committed) once a live acked sourceable-holder
//     quorum exists for its durability class, THEN fires the product OnCommitted
//     hook exactly once; or
//   - fails it (staging → failed) once it has been staging longer than FailAfter
//     with the quorum still unmet (a permanent miss).
//
// All durability state lives in the DB (commit_state), so the reconciler is
// crash-safe: a restart re-scans the same staging rows and resumes. There is no
// in-memory attempt counter — failure is age-based (now - updated_at > FailAfter).
// The pass is idempotent: MarkCommitted's WHERE commit_state='staging' guard
// means a re-run on an already-committed row affects 0 rows, so OnCommitted
// fires exactly once (gated on rows == 1).
type Reconciler struct {
	q    *gen.Queries
	hook WriteHook
	gate *CommitGateConfig
	log  *slog.Logger
}

// NewReconciler builds a Reconciler from a storage Service. It returns nil when
// the gate is off (no commitGate, or RequireQuorum=false) — there is nothing to
// reconcile, so the caller skips starting the loop. The Service supplies the
// shared *gen.Queries, the product hook, and the gate config.
func NewReconciler(s *Service) *Reconciler {
	if s == nil || s.commitGate == nil || !s.commitGate.RequireQuorum {
		return nil
	}
	return &Reconciler{
		q:    s.q,
		hook: s.hook,
		gate: s.commitGate,
		log:  slog.Default(),
	}
}

// Run loops on the gate's ReconcilerInterval, calling reconcileOnce each tick,
// until ctx is cancelled. It performs one immediate pass on entry so a freshly
// started coordinator does not wait a full interval to drain staging rows.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.gate.ReconcilerInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	r.reconcileOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reconcileOnce(ctx)
		}
	}
}

// ReconcileOnce runs exactly one reconciliation pass and returns. It is a thin
// delegation to the unexported reconcileOnce, exposed as a test/ops seam so an
// operator runbook or an end-to-end test can drive a single deterministic pass
// without standing up the Run ticker goroutine. Production uses Run.
func (r *Reconciler) ReconcileOnce(ctx context.Context) { r.reconcileOnce(ctx) }

// reconcileOnce performs a single reconciliation pass over the staging backlog.
// It is safe to call concurrently with itself only in the trivial sense that the
// MarkCommitted/MarkFailed guards make double-processing a no-op; the intended
// caller is the single Run goroutine.
func (r *Reconciler) reconcileOnce(ctx context.Context) {
	rows, err := r.q.ListStagingBlobs(ctx, reconcileBatchSize)
	if err != nil {
		r.log.Warn("storage.commit.reconcile_list_failed", "err", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		r.reconcileBlob(ctx, row)
	}
}

// reconcileBlob processes one staging blob: commit-on-quorum, fail-on-age, or
// leave staging for the next tick.
func (r *Reconciler) reconcileBlob(ctx context.Context, row gen.ListStagingBlobsRow) {
	class := row.DurabilityClass
	want := r.gate.QuorumFor(class)

	held, err := r.q.CountSourceableHolders(ctx, gen.CountSourceableHoldersParams{
		Cid: row.Cid, StaleSecs: r.gate.StaleSeconds,
	})
	if err != nil {
		r.log.Warn("storage.commit.count_holders_failed", "cid", row.Cid, "err", err)
		return
	}

	if int(held) >= want {
		r.commit(ctx, row)
		return
	}

	// Quorum still unmet. Fail the blob only once it has out-aged FailAfter;
	// otherwise leave it staging so the next tick re-checks.
	if r.gate.FailAfter > 0 && time.Since(row.UpdatedAt) > r.gate.FailAfter {
		n, err := r.q.MarkFailed(ctx, row.Cid)
		if err != nil {
			r.log.Warn("storage.commit.mark_failed_failed", "cid", row.Cid, "err", err)
			return
		}
		if n == 1 {
			r.log.Warn("storage.commit.failed", "cid", row.Cid, "class", class, "held", held, "want", want)
		}
	}
}

// commit flips a quorum-met staging blob to committed and, only if this pass is
// the one that performed the flip (MarkCommitted rows == 1), fires the deferred
// OnCommitted hook. The rows == 1 guard is the idempotency/crash-safety pivot:
// a concurrent pass or a post-crash re-run sees commit_state already 'committed',
// affects 0 rows, and does NOT re-fire the hook.
func (r *Reconciler) commit(ctx context.Context, row gen.ListStagingBlobsRow) {
	size, err := r.q.GetBlobSize(ctx, row.Cid)
	if err != nil {
		r.log.Warn("storage.commit.get_size_failed", "cid", row.Cid, "err", err)
		return
	}
	n, err := r.q.MarkCommitted(ctx, gen.MarkCommittedParams{Cid: row.Cid, LocalBytes: size})
	if err != nil {
		r.log.Warn("storage.commit.mark_committed_failed", "cid", row.Cid, "err", err)
		return
	}
	if n != 1 {
		// Already committed by an earlier pass — idempotent no-op, no hook.
		return
	}

	ref := CommittedRef{CID: row.Cid, Product: string(row.Product)}
	// Best-effort visibility for the product hook; a lookup failure must not
	// undo the commit (the row is already committed). Default to private.
	if vis, verr := r.q.ResolveEffectiveVisibility(ctx, row.Cid); verr == nil {
		ref.Visibility = resolveVisibility(vis)
	} else {
		r.log.Warn("storage.commit.resolve_visibility_failed", "cid", row.Cid, "err", verr)
	}
	if r.hook != nil {
		r.hook.OnCommitted(ctx, ref)
	}
	r.log.Info("storage.commit.committed", "cid", row.Cid, "class", row.DurabilityClass, "product", ref.Product)
}
