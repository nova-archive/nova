package storage

import (
	"context"
	"io"
	"log/slog"
	"time"

	gocid "github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/ipfs"
)

// StorageMode selects how the coordinator treats locally-pinned blobs on the
// read path (coordinator_storage_mode, D-M4.1-9). The default is origin_copy.
type StorageMode string

const (
	// StorageModeOriginCopy keeps every donor-fetched blob pinned forever: the
	// coordinator is a full origin copy. The cache eviction loop is a no-op.
	// (Origin pruning is a separate, durability-driven mechanism — Task 12.)
	StorageModeOriginCopy StorageMode = "origin_copy"

	// StorageModeBoundedCache caps locally-cached donor-fetched bytes at
	// MaxBytes using a size-aware SLRU/2Q policy: new objects land probationary,
	// a cache hit promotes to protected, and eviction drains probationary
	// oldest-first so a scan over Nova's large immutable blobs cannot pollute
	// the hot (protected) set.
	StorageModeBoundedCache StorageMode = "bounded_cache"

	// StorageModeTransient holds nothing beyond the in-flight read: a
	// donor-fetched blob is unpinned as soon as the read is served, so the next
	// committed read is donor-backed again. (Useful for a coordinator that is a
	// pure routing/decrypt tier with no local durability role.)
	StorageModeTransient StorageMode = "transient"
)

// Default cache tunables. Mirror config.Coordinator defaults; both must stay in
// sync (see internal/config/types.go).
const (
	defaultBoundedCacheProtectedRatio = 0.80
	defaultLruTouchInterval           = 60 * time.Second
)

// StorageModeConfig carries the normalized coordinator_storage_mode tunables.
// It is installed once via WithStorageMode (the mode config is available at
// construction, unlike the donor tier's deferred mTLS material). Zero values
// are normalized to documented defaults by withDefaults so a partially-filled
// struct (e.g. from a config block) is still safe.
type StorageModeConfig struct {
	Mode StorageMode // origin_copy (default) | bounded_cache | transient

	// MaxBytes is the bounded_cache total budget (probationary + protected). A
	// non-positive value in bounded_cache mode disables eviction (treated as
	// unbounded — operator misconfiguration; the validator in Task 14 will warn).
	MaxBytes int64

	// ProtectedRatio caps the protected segment at ProtectedRatio × MaxBytes so
	// the probationary tier always retains admission headroom. Default 0.80.
	ProtectedRatio float64

	// MaxObjectBytes refuses cache admission of any single object larger than
	// this (the bytes are still served, just not cached). 0 ⇒ no per-object
	// ceiling.
	MaxObjectBytes int64

	// TouchInterval throttles last_accessed_at bumps and protected-promotions:
	// a second access within this window is a no-op so hot reads do not churn
	// the DB. Default 60s.
	TouchInterval time.Duration
}

// withDefaults normalizes a StorageModeConfig: an empty/unknown mode becomes
// origin_copy, and the ratio/touch knobs take their documented defaults when
// non-positive.
func (c StorageModeConfig) withDefaults() StorageModeConfig {
	switch c.Mode {
	case StorageModeOriginCopy, StorageModeBoundedCache, StorageModeTransient:
		// keep
	default:
		c.Mode = StorageModeOriginCopy
	}
	if c.ProtectedRatio <= 0 || c.ProtectedRatio > 1 {
		c.ProtectedRatio = defaultBoundedCacheProtectedRatio
	}
	if c.TouchInterval <= 0 {
		c.TouchInterval = defaultLruTouchInterval
	}
	return c
}

// evictionBatch bounds one ListEvictionCandidates page; evictToFit loops until
// the budget is met or a pass evicts nothing.
const evictionBatch = 64

// cachePolicy implements the size-aware SLRU/2Q bounded cache. It runs against
// the REAL SQL projection (*gen.Queries) so the probationary→protected segment
// semantics and the eviction ORDER BY are exactly the DB's. q may be nil in
// transient mode (transient holds nothing and issues no admission/eviction
// queries) and in the DB-free wrapper-timing unit test.
type cachePolicy struct {
	q       *gen.Queries
	backend ipfs.Backend
	cfg     StorageModeConfig
}

// newCachePolicyFor builds a cachePolicy over the given queries/backend and
// normalizes cfg. Exposed for the storage package's own tests; production code
// installs it via WithStorageMode.
func newCachePolicyFor(q *gen.Queries, backend ipfs.Backend, cfg StorageModeConfig) *cachePolicy {
	return &cachePolicy{q: q, backend: backend, cfg: cfg.withDefaults()}
}

// mode returns the normalized storage mode.
func (p *cachePolicy) mode() StorageMode { return p.cfg.Mode }

// admit records a freshly donor-fetched, verified-and-pinned blob in the cache
// projection and (in bounded_cache) enforces the byte budget via SLRU eviction.
//
//   - origin_copy: admit to the projection (cache role) but never evict.
//   - bounded_cache: admit to probationary, then evictToFit. Objects larger
//     than MaxObjectBytes are NOT cached (the bytes were already served; we just
//     do not pin them as a cache entry — admission is refused).
//   - transient: no-op (the blob is about to be unpinned; it is never a present
//     cache entry).
//
// All errors are best-effort logged — a failed admit/evict must not fail the
// read, whose bytes are already pinned and served. All byte accounting uses
// envelope_size (envSize / local_bytes), D-M4.1-16.
func (p *cachePolicy) admit(ctx context.Context, cidStr string, envSize int64) {
	if p.cfg.Mode == StorageModeTransient {
		// Transient holds nothing as a present cache entry.
		return
	}
	if p.q == nil {
		return
	}
	if p.cfg.MaxObjectBytes > 0 && envSize > p.cfg.MaxObjectBytes {
		// Refuse admission of an oversize object. The bytes were already served;
		// we simply do not cache them (no AdmitToCache, no row).
		slog.Info("storage.cache.admit_refused", "cid", cidStr, "bytes", envSize,
			"max_object_bytes", p.cfg.MaxObjectBytes)
		return
	}
	if err := p.q.AdmitToCache(ctx, gen.AdmitToCacheParams{
		Cid:             cidStr,
		DurabilityClass: "cache",
		LocalBytes:      envSize,
	}); err != nil {
		slog.Warn("storage.cache.admit_failed", "cid", cidStr, "err", err)
		return
	}
	if p.cfg.Mode == StorageModeBoundedCache {
		p.evictToFit(ctx)
	}
}

// onHit is called on a local cache hit. It throttle-bumps last_accessed_at and,
// for a probationary object, throttle-promotes it to protected (the second
// access promotes — the scan-pollution defense). Both queries are no-ops if the
// row was touched within TouchInterval (hot reads must not churn the DB).
// origin_copy still touches/promotes (harmless bookkeeping); transient never
// reaches here (it holds no present cache entry).
func (p *cachePolicy) onHit(ctx context.Context, cidStr string) {
	if p.q == nil || p.cfg.Mode == StorageModeTransient {
		return
	}
	threshold := pgtype.Timestamptz{Time: time.Now().Add(-p.cfg.TouchInterval), Valid: true}
	// Order matters: PromoteToProtected runs FIRST. Both queries gate on the same
	// last_accessed_at < threshold throttle, and TouchLastAccessed bumps
	// last_accessed_at to now(); running it first would suppress the very promote
	// it is meant to accompany. Promote (also a last_accessed_at=now() bump)
	// itself satisfies the touch, and the subsequent TouchLastAccessed is then a
	// throttled no-op — so a stale probationary row is promoted on this hit and a
	// stale protected row is still touched (promote matches nothing, touch bumps).
	if err := p.q.PromoteToProtected(ctx, gen.PromoteToProtectedParams{
		Cid:         cidStr,
		ThresholdAt: threshold,
	}); err != nil {
		slog.Warn("storage.cache.promote_failed", "cid", cidStr, "err", err)
	}
	if err := p.q.TouchLastAccessed(ctx, gen.TouchLastAccessedParams{
		Cid:         cidStr,
		ThresholdAt: threshold,
	}); err != nil {
		slog.Warn("storage.cache.touch_failed", "cid", cidStr, "err", err)
	}
}

// evictToFit enforces the bounded_cache byte budget in two phases:
//
//  1. Protected-ratio cap: while protected_bytes > ProtectedRatio × MaxBytes,
//     evict the oldest protected entry. (The brief's "demote oldest protected"
//     is implemented as EVICTION — there is no demote-to-probationary query and
//     ListEvictionCandidates' ordering supports oldest-protected-first.)
//  2. Total cap: while probationary_bytes + protected_bytes > MaxBytes, evict in
//     ListEvictionCandidates order (probationary oldest-first; protected only
//     after probationary is exhausted).
//
// A non-positive MaxBytes disables eviction (treated as unbounded). Each pass
// that evicts nothing breaks the loop to guard against spinning when nothing is
// evictable.
func (p *cachePolicy) evictToFit(ctx context.Context) {
	if p.cfg.Mode != StorageModeBoundedCache || p.q == nil || p.cfg.MaxBytes <= 0 {
		return
	}
	protectedCap := int64(p.cfg.ProtectedRatio * float64(p.cfg.MaxBytes))

	// Phase 1: protected-ratio cap. Evict oldest protected entries until
	// protected_bytes is within the cap.
	for {
		sums, err := p.q.SumCacheBytes(ctx)
		if err != nil {
			slog.Warn("storage.cache.sum_failed", "err", err)
			return
		}
		if sums.ProtectedBytes <= protectedCap {
			break
		}
		if !p.evictOldestProtected(ctx) {
			break // nothing protected evictable this pass
		}
	}

	// Phase 2: total cap. Evict in ListEvictionCandidates order until total
	// bytes are within MaxBytes.
	for {
		sums, err := p.q.SumCacheBytes(ctx)
		if err != nil {
			slog.Warn("storage.cache.sum_failed", "err", err)
			return
		}
		if sums.ProbationaryBytes+sums.ProtectedBytes <= p.cfg.MaxBytes {
			break
		}
		cands, err := p.q.ListEvictionCandidates(ctx, evictionBatch)
		if err != nil {
			slog.Warn("storage.cache.list_candidates_failed", "err", err)
			return
		}
		if len(cands) == 0 {
			break
		}
		// Evict in order until the budget is met (re-check after each so we do
		// not over-evict a whole batch).
		evictedAny := false
		total := sums.ProbationaryBytes + sums.ProtectedBytes
		for _, cand := range cands {
			if total <= p.cfg.MaxBytes {
				break
			}
			if p.evict(ctx, cand.Cid) {
				total -= cand.LocalBytes
				evictedAny = true
			}
		}
		if !evictedAny {
			break // nothing in this batch was evictable; avoid an infinite loop
		}
	}
}

// evictOldestProtected evicts the single oldest protected entry. It returns true
// when an entry was evicted, false when there are no protected candidates (so
// the caller's loop terminates). ListEvictionCandidates orders probationary
// before protected, so it scans the page for the first protected row.
func (p *cachePolicy) evictOldestProtected(ctx context.Context) bool {
	cands, err := p.q.ListEvictionCandidates(ctx, evictionBatch)
	if err != nil {
		slog.Warn("storage.cache.list_candidates_failed", "err", err)
		return false
	}
	for _, cand := range cands {
		if cand.CacheSegment.Valid && cand.CacheSegment.CacheSegment == gen.CacheSegmentProtected {
			return p.evict(ctx, cand.Cid)
		}
	}
	return false
}

// evict unpins the blob in the backend and marks the projection row absent
// (local_present=false, role=absent, cache_segment=NULL, local_bytes=0). It
// returns true on a successful state update. Unpin errors are logged but do not
// abort the projection update — a stale pin is reclaimable later, but the
// budget must reflect the eviction. (Origin PRUNING is Task 12; this only
// touches cache-role rows that ListEvictionCandidates selected.)
func (p *cachePolicy) evict(ctx context.Context, cidStr string) bool {
	if c, err := gocid.Decode(cidStr); err != nil {
		slog.Warn("storage.cache.evict_decode_failed", "cid", cidStr, "err", err)
	} else if uerr := p.backend.Unpin(ctx, c); uerr != nil {
		slog.Warn("storage.cache.unpin_failed", "cid", cidStr, "err", uerr)
	}
	if err := p.q.SetLocalPresence(ctx, gen.SetLocalPresenceParams{
		Cid:             cidStr,
		LocalPresent:    false,
		LocalRole:       gen.CoordinatorLocalRoleAbsent,
		CacheSegment:    gen.NullCacheSegment{Valid: false},
		LocalBytes:      0,
		PruneEligibleAt: pgtype.Timestamptz{Valid: false},
	}); err != nil {
		slog.Warn("storage.cache.set_absent_failed", "cid", cidStr, "err", err)
		return false
	}
	slog.Info("storage.cache.evicted", "cid", cidStr)
	return true
}

// unpinBlob unpins a single blob in the backend, best-effort. Used by the
// transient-mode OpenBytes path to release a donor-fetched pin after the read is
// served. Errors are logged — a stale pin is reclaimable, and the read already
// succeeded.
func (p *cachePolicy) unpinBlob(ctx context.Context, c gocid.Cid) {
	if err := p.backend.Unpin(ctx, c); err != nil {
		slog.Warn("storage.cache.transient_unpin_failed", "cid", c.String(), "err", err)
	}
}

// unpinOnClose wraps a streamed reader so the donor-fetched pin is released when
// the consumer closes the reader. Used by the transient-mode public_archival
// path, where OpenBytes returns the backend stream directly (no buffering).
type unpinOnClose struct {
	io.ReadCloser
	unpin func()
}

func (u unpinOnClose) Close() error {
	err := u.ReadCloser.Close()
	u.unpin()
	return err
}
