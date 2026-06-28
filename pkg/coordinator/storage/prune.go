package storage

import (
	"context"
	"log/slog"
	"time"

	gocid "github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/ipfs"
)

// pruneBatchSize bounds one ListPruneCandidates page so a large backlog is
// drained over several ticks rather than in one long sweep.
const pruneBatchSize = 256

// originClasses are the durability classes that the origin pruner targets.
// "cache" is intentionally excluded — cache-role eviction is Task 9's domain.
var originClasses = []string{"important", "normal"}

// PrunerConfig carries the tunables for the origin pruner.
type PrunerConfig struct {
	// Floor is the minimum live acked DONOR holder count required before an
	// origin blob may be pruned (unpin + mark absent). When the holder count is
	// < Floor the local copy is retained. Default 2.
	Floor int

	// StaleSeconds is the age window for CountSourceableHolders: nodes
	// last-seen older than this are excluded (treated as not live). Default
	// mirrors the federation donor-freshness window (3600s).
	StaleSeconds float64

	// Interval is the sleep between pruneOnce passes. Default 60s.
	Interval time.Duration
}

// withPrunerDefaults normalizes a PrunerConfig: zero/negative values are
// replaced with sensible defaults.
func (c PrunerConfig) withPrunerDefaults() PrunerConfig {
	if c.Floor <= 0 {
		c.Floor = 2
	}
	if c.StaleSeconds <= 0 {
		c.StaleSeconds = 3600
	}
	if c.Interval <= 0 {
		c.Interval = 60 * time.Second
	}
	return c
}

// Pruner is the P2-M4.1 origin durability pruner. It periodically scans
// committed, locally-present origin blobs ('important' and 'normal' durability
// class) and, for each:
//
//  1. Crash-window reconcile: if the blockstore has already lost the blob
//     (unpin crashed mid-way or GC ran), align the projection to reality
//     (mark absent) and, if the holder count is < Floor, log a durability
//     alert.
//  2. Prune-or-retain: if the blob is present AND live acked donor holders
//     >= Floor, unpin it (the donors are now the durable substrate). Otherwise,
//     retain the local copy — never prune below the floor.
//
// In bounded_cache mode, pruneOnce also triggers Task-9 size eviction
// (cache.evictToFit) so the two mechanisms compose cleanly.
//
// The invariant that makes this safe: CountSourceableHolders counts ONLY donor
// acked holders (pin_assignments state='acked' on nodes). The coordinator's
// own local copy (origin OR cache) is never in that count, so "cache never
// counts as a replica" is automatic — do not add local presence to the count.
//
// NewPruner returns nil when s.cache is nil or the mode is origin_copy:
// origin_copy never prunes (the coordinator IS the origin store); nil is the
// zero-cost guard for callers.
type Pruner struct {
	q        *gen.Queries
	backend  ipfs.Backend
	cache    *cachePolicy // nil iff transient-mode (no cache size tracking needed)
	floor    int
	stale    float64
	interval time.Duration
	log      *slog.Logger
}

// NewPruner builds a Pruner from a storage Service. Returns nil when:
//   - s.cache == nil: no storage mode policy installed.
//   - s.cache.mode() == StorageModeOriginCopy: origin_copy keeps every blob; pruning is disabled.
func NewPruner(s *Service, cfg PrunerConfig) *Pruner {
	if s == nil || s.cache == nil {
		return nil
	}
	if s.cache.mode() == StorageModeOriginCopy {
		return nil
	}
	cfg = cfg.withPrunerDefaults()
	return &Pruner{
		q:        s.q,
		backend:  s.backend,
		cache:    s.cache,
		floor:    cfg.Floor,
		stale:    cfg.StaleSeconds,
		interval: cfg.Interval,
		log:      slog.Default(),
	}
}

// Run loops on the configured interval, calling pruneOnce each tick, until ctx
// is cancelled. It performs one immediate pass on entry so a freshly started
// coordinator does not wait a full interval to drain prune-eligible rows.
func (p *Pruner) Run(ctx context.Context) {
	p.pruneOnce(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pruneOnce(ctx)
		}
	}
}

// pruneOnce performs a single origin-pruning pass over both origin durability
// classes ('important', 'normal'), then (in bounded_cache mode) triggers
// Task-9 cache size eviction so the two mechanisms compose cleanly.
func (p *Pruner) pruneOnce(ctx context.Context) {
	log := p.log
	if log == nil {
		log = slog.Default()
	}

	for _, class := range originClasses {
		cands, err := p.q.ListPruneCandidates(ctx, gen.ListPruneCandidatesParams{
			DurabilityClass: class,
			Lim:             pruneBatchSize,
		})
		if err != nil {
			log.Warn("storage.prune.list_candidates_failed", "class", class, "err", err)
			continue
		}

		for _, cand := range cands {
			if ctx.Err() != nil {
				return
			}
			p.pruneCandidate(ctx, cand, log)
		}
	}

	// After processing origin classes, in bounded_cache mode trigger Task-9
	// size eviction so cache-role rows are drained within the byte budget.
	if p.cache != nil && p.cache.mode() == StorageModeBoundedCache {
		p.cache.evictToFit(ctx)
	}
}

// pruneCandidate processes one ListPruneCandidates row. The algorithm is:
//
//  1. Decode the CID string (required for the backend calls).
//  2. Crash-window reconcile: check whether the blockstore actually has the
//     blob. The DB says local_present=true (query filter), but the blockstore
//     may have lost it (unpin crashed mid-way, or a GC ran).
//  3. If the backend is missing the blob: align the projection (mark absent),
//     log an alert if we are also under-replicated, and continue.
//  4. If the backend has the blob AND live holder count >= floor: Unpin + mark
//     absent (prune applied).
//  5. If the backend has the blob AND live holder count < floor: retain the
//     local copy (log the skipped-floor event).
func (p *Pruner) pruneCandidate(ctx context.Context, cand gen.ListPruneCandidatesRow, log *slog.Logger) {
	c2, err := gocid.Decode(cand.Cid)
	if err != nil {
		log.Warn("storage.prune.decode_failed", "cid", cand.Cid, "err", err)
		return
	}

	has, err := p.backend.Has(ctx, c2)
	if err != nil {
		log.Warn("storage.prune.has_failed", "cid", cand.Cid, "err", err)
		return
	}

	// CRASH-WINDOW RECONCILE: the candidate was local_present=true in the DB,
	// but the backend may have lost it already (unpin crashed mid-way, or GC ran).
	if !has {
		// Align projection to reality: mark absent.
		if serr := p.setAbsent(ctx, cand.Cid); serr != nil {
			log.Warn("storage.prune.set_absent_failed", "cid", cand.Cid, "err", serr)
		}
		// Count live donors so we can alert if under-replicated.
		n, cerr := p.q.CountSourceableHolders(ctx, gen.CountSourceableHoldersParams{
			Cid: cand.Cid, StaleSecs: p.stale,
		})
		if cerr != nil {
			log.Warn("storage.prune.count_holders_failed", "cid", cand.Cid, "err", cerr)
		} else if int(n) < p.floor {
			// We are absent AND under-replicated — durability at risk.
			// Do NOT inflate (cache is not a replica). Log the alert only.
			log.Warn("storage.prune.below_floor_alert",
				"cid", cand.Cid, "holders", n, "floor", p.floor,
				"durability_class", cand.DurabilityClass)
		}
		return
	}

	// Backend has the blob. Count live donor holders to decide prune vs retain.
	n, err := p.q.CountSourceableHolders(ctx, gen.CountSourceableHoldersParams{
		Cid: cand.Cid, StaleSecs: p.stale,
	})
	if err != nil {
		log.Warn("storage.prune.count_holders_failed", "cid", cand.Cid, "err", err)
		return
	}

	if int(n) >= p.floor {
		// Safe to prune: donors are the durable substrate.
		if uerr := p.backend.Unpin(ctx, c2); uerr != nil {
			log.Warn("storage.prune.unpin_failed", "cid", cand.Cid, "err", uerr)
			// Continue to update the projection — a stale pin is reclaimable later,
			// but the DB should reflect our intent.
		}
		if serr := p.setAbsent(ctx, cand.Cid); serr != nil {
			log.Warn("storage.prune.set_absent_failed", "cid", cand.Cid, "err", serr)
			return
		}
		log.Info("storage.prune.applied",
			"cid", cand.Cid, "holders", n,
			"bytes_reclaimed", cand.LocalBytes,
			"durability_class", cand.DurabilityClass)
	} else {
		// Not enough donors yet — retain the local origin copy.
		log.Info("storage.prune.skipped_floor",
			"cid", cand.Cid, "holders", n, "floor", p.floor,
			"durability_class", cand.DurabilityClass)
	}
}

// reconcilePresence aligns the local-presence projection to the blockstore in
// BOTH directions, for the crash-window cases:
//
//   - Projection says present, backend missing ⇒ mark projection absent (the
//     unpin already happened; correct the stale row).
//   - Projection says absent, backend present ⇒ Unpin the orphaned pin (reclaim
//     reclaimable bytes; the bytes are donor-re-fetchable). The projection stays
//     absent — this does NOT re-adopt the blob as a cache or origin replica.
//
// This method is exported for unit-testing; pruneOnce inlines the
// present-direction reconcile as a fast-path (it has the Has result already).
func (p *Pruner) reconcilePresence(ctx context.Context, cidStr string) error {
	log := p.log
	if log == nil {
		log = slog.Default()
	}

	c2, err := gocid.Decode(cidStr)
	if err != nil {
		return err
	}

	st, err := p.q.GetStorageState(ctx, cidStr)
	if err != nil {
		return err
	}

	has, err := p.backend.Has(ctx, c2)
	if err != nil {
		return err
	}

	switch {
	case st.LocalPresent && !has:
		// Projection says present but backend has lost the bytes.
		// Align the projection to reality.
		log.Info("storage.prune.reconcile_mark_absent",
			"cid", cidStr, "reason", "projection_present_backend_missing")
		return p.setAbsent(ctx, cidStr)

	case !st.LocalPresent && has:
		// Projection says absent but the backend still has an orphaned pin.
		// Reclaim the bytes via Unpin. Do NOT mark present — the coordinator
		// does not re-adopt this as a replica (cache is never a replica; the
		// original unpin intent is what we preserve). Projection stays absent.
		log.Info("storage.prune.reconcile_unpin_orphan",
			"cid", cidStr, "reason", "projection_absent_backend_present")
		if uerr := p.backend.Unpin(ctx, c2); uerr != nil {
			log.Warn("storage.prune.reconcile_unpin_failed", "cid", cidStr, "err", uerr)
			return uerr
		}
		// No projection update needed: it already says absent.
		return nil

	default:
		// Both agree — nothing to do.
		return nil
	}
}

// setAbsent updates the blob_storage_state row to local_present=false,
// role=absent, cache_segment=NULL, local_bytes=0, prune_eligible_at=NULL.
func (p *Pruner) setAbsent(ctx context.Context, cidStr string) error {
	return p.q.SetLocalPresence(ctx, gen.SetLocalPresenceParams{
		Cid:             cidStr,
		LocalPresent:    false,
		LocalRole:       gen.CoordinatorLocalRoleAbsent,
		CacheSegment:    gen.NullCacheSegment{Valid: false},
		LocalBytes:      0,
		PruneEligibleAt: pgtype.Timestamptz{Valid: false},
	})
}
