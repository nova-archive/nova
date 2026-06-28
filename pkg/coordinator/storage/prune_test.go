package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// seedOriginBlob inserts a blobs + blob_manifests row and a committed/origin/
// local_present blob_storage_state row — the typical state of a blob that the
// coordinator wrote and which donors have replicated. local_bytes is set to
// envSize so ListPruneCandidates returns a meaningful bytes_reclaimed figure.
// The backend is also populated so Has() returns true.
func seedOriginBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, be *echoBackend, cidStr string, envSize int64, class string) {
	t.Helper()
	// blobs FK
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ($1, 'application/octet-stream', $2, 'active', 'raw', 2)
		ON CONFLICT (cid) DO NOTHING
	`, cidStr, envSize)
	require.NoError(t, err)
	// blob_manifests FK
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		VALUES ($1, 'sha2-256', 'raw', 'size-262144', $2, $2, 1)
		ON CONFLICT (cid) DO NOTHING
	`, cidStr, envSize)
	require.NoError(t, err)
	// committed origin row — this is the state after MarkCommitted runs
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_storage_state
			(cid, commit_state, durability_class, local_role, local_present, local_bytes, updated_at)
		VALUES ($1, 'committed', $2, 'origin', true, $3, now())
		ON CONFLICT (cid) DO UPDATE SET
			commit_state = 'committed',
			durability_class = EXCLUDED.durability_class,
			local_role = 'origin',
			local_present = true,
			local_bytes = EXCLUDED.local_bytes,
			updated_at = now()
	`, cidStr, class, envSize)
	require.NoError(t, err)
	// populate backend so Has() returns true
	payload := []byte("nova-origin-blob:" + cidStr)
	be.put(cidStr, payload)
}

// seedOriginBlobAbsent is like seedOriginBlob but does NOT put bytes in the
// backend (simulates a blob whose backend copy is already gone — crash-window
// or manual GC scenario). The DB row is still local_present=true (stale projection).
func seedOriginBlobAbsent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string, envSize int64, class string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ($1, 'application/octet-stream', $2, 'active', 'raw', 2)
		ON CONFLICT (cid) DO NOTHING
	`, cidStr, envSize)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		VALUES ($1, 'sha2-256', 'raw', 'size-262144', $2, $2, 1)
		ON CONFLICT (cid) DO NOTHING
	`, cidStr, envSize)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_storage_state
			(cid, commit_state, durability_class, local_role, local_present, local_bytes, updated_at)
		VALUES ($1, 'committed', $2, 'origin', true, $3, now())
		ON CONFLICT (cid) DO UPDATE SET
			commit_state = 'committed',
			durability_class = EXCLUDED.durability_class,
			local_role = 'origin',
			local_present = true,
			local_bytes = EXCLUDED.local_bytes,
			updated_at = now()
	`, cidStr, class, envSize)
	require.NoError(t, err)
}

// seedAbsentProjection inserts a row with local_present=false (and no backend
// bytes) but leaves a pin in the backend — simulating an orphaned backend pin
// where the DB projection says absent but the blockstore still has it.
func seedAbsentProjection(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string, envSize int64, class string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ($1, 'application/octet-stream', $2, 'active', 'raw', 2)
		ON CONFLICT (cid) DO NOTHING
	`, cidStr, envSize)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_storage_state
			(cid, commit_state, durability_class, local_role, local_present, local_bytes, updated_at)
		VALUES ($1, 'committed', $2, 'absent', false, 0, now())
		ON CONFLICT (cid) DO UPDATE SET
			commit_state = 'committed',
			local_role = 'absent',
			local_present = false,
			local_bytes = 0,
			updated_at = now()
	`, cidStr, class)
	require.NoError(t, err)
}

// newPrunerForTest builds a Pruner directly from components (no coordinator
// wiring needed for unit-level tests).
func newPrunerForTest(pool *pgxpool.Pool, be *echoBackend, cache *cachePolicy, floor int, stale float64, interval time.Duration) *Pruner {
	return &Pruner{
		q:        gen.New(pool),
		backend:  be,
		cache:    cache,
		floor:    floor,
		stale:    stale,
		interval: interval,
		log:      nil, // nil = slog.Default() will be used
	}
}

// TestPrunerPrunesAtOrAboveFloor: an origin blob present locally with >= floor
// acked sourceable holders ⇒ after pruneOnce: local_present=false, role=absent,
// AND the echoBackend no longer has it.
func TestPrunerPrunesAtOrAboveFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	payload := []byte("pruner-at-floor-test")
	cidStr := mkRawCID(t, payload)
	const envSize = 512

	seedOriginBlob(t, ctx, pool, be, cidStr, envSize, "important")
	require.True(t, be.has(cidStr), "backend must have the blob before pruning")

	// Seed floor=2 acked sourceable holders.
	node1, node2 := uuid.New(), uuid.New()
	seedSourceableNode(t, ctx, pool, node1)
	seedSourceableNode(t, ctx, pool, node2)
	seedAckedHolder(t, ctx, pool, cidStr, node1)
	seedAckedHolder(t, ctx, pool, cidStr, node2)

	p := newPrunerForTest(pool, be, nil, 2, 3600, 60*time.Second)
	p.pruneOnce(ctx)

	st := getState(t, ctx, pool, cidStr)
	require.False(t, st.LocalPresent, "after pruning at-floor: local_present must be false")
	require.Equal(t, gen.CoordinatorLocalRoleAbsent, st.LocalRole, "after pruning: role must be absent")
	require.False(t, st.CacheSegment.Valid, "after pruning: cache_segment must be NULL")
	require.EqualValues(t, 0, st.LocalBytes, "after pruning: local_bytes must be 0")
	require.False(t, be.has(cidStr), "after pruning: blob must be unpinned in the backend")
}

// TestPrunerNeverBelowFloorRetains: origin blob present with < floor holders
// ⇒ after pruneOnce: still local_present=true AND still pinned in the backend.
func TestPrunerNeverBelowFloorRetains(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	payload := []byte("pruner-below-floor-retain-test")
	cidStr := mkRawCID(t, payload)
	const envSize = 256

	seedOriginBlob(t, ctx, pool, be, cidStr, envSize, "important")

	// Only 1 holder — below floor=2.
	node1 := uuid.New()
	seedSourceableNode(t, ctx, pool, node1)
	seedAckedHolder(t, ctx, pool, cidStr, node1)

	p := newPrunerForTest(pool, be, nil, 2, 3600, 60*time.Second)
	p.pruneOnce(ctx)

	st := getState(t, ctx, pool, cidStr)
	require.True(t, st.LocalPresent, "below-floor: local_present must remain true (never prune below floor)")
	require.Equal(t, gen.CoordinatorLocalRoleOrigin, st.LocalRole, "below-floor: role must remain origin")
	require.True(t, be.has(cidStr), "below-floor: blob must remain pinned in backend")
}

// TestPrunerBelowFloorAbsentAlertsAndHalts: a committed origin blob whose
// backend copy is MISSING + < floor holders ⇒ pruneOnce marks it absent and
// does NOT inflate replica count. Assert local_present=false, role=absent, no
// extra state implying a replica.
func TestPrunerBelowFloorAbsentAlertsAndHalts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	payload := []byte("pruner-below-floor-absent-test")
	cidStr := mkRawCID(t, payload)
	const envSize = 128

	// DB says local_present=true but the backend doesn't have it.
	seedOriginBlobAbsent(t, ctx, pool, cidStr, envSize, "important")
	require.False(t, be.has(cidStr), "backend must NOT have the blob (crash scenario)")

	// Only 1 holder — below floor=2 (under-replicated AND locally absent).
	node1 := uuid.New()
	seedSourceableNode(t, ctx, pool, node1)
	seedAckedHolder(t, ctx, pool, cidStr, node1)

	p := newPrunerForTest(pool, be, nil, 2, 3600, 60*time.Second)
	p.pruneOnce(ctx)

	// Projection should be corrected to absent (crash-window reconcile).
	st := getState(t, ctx, pool, cidStr)
	require.False(t, st.LocalPresent, "absent alert path: local_present must be false (projection corrected)")
	require.Equal(t, gen.CoordinatorLocalRoleAbsent, st.LocalRole, "absent alert path: role must be absent")
	require.EqualValues(t, 0, st.LocalBytes, "absent alert path: local_bytes must be 0")
	// Must NOT have inflated the cache or pinned as a replica.
	require.False(t, st.CacheSegment.Valid, "absent alert path: cache_segment must be NULL (no replica inflation)")
	require.False(t, be.has(cidStr), "absent alert path: backend still does not have it (no re-fetch)")
}

// TestPrunerReconcilesCrashWindow exercises reconcilePresence in both directions:
// (a) projection says present but backend is missing ⇒ mark absent.
// (b) projection says absent but backend has it (orphaned pin) ⇒ Unpin and
//
//	leave projection absent (reclaim orphan, do not re-adopt as replica).
func TestPrunerReconcilesCrashWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	// (a) Present projection, backend missing.
	payloadA := []byte("reconcile-crash-a: present projection, missing backend")
	cidA := mkRawCID(t, payloadA)
	seedOriginBlobAbsent(t, ctx, pool, cidA, 64, "important")
	require.False(t, be.has(cidA), "setup: cidA must not be in backend")

	// (b) Absent projection, backend has an orphaned pin.
	payloadB := []byte("reconcile-crash-b: absent projection, orphaned backend pin")
	cidB := mkRawCID(t, payloadB)
	seedAbsentProjection(t, ctx, pool, cidB, 64, "important")
	be.put(cidB, payloadB) // backend has it; projection says absent

	p := newPrunerForTest(pool, be, nil, 2, 3600, 60*time.Second)

	// (a): reconcile present-projection / backend-missing ⇒ projection corrected
	err := p.reconcilePresence(ctx, cidA)
	require.NoError(t, err)
	stA := getState(t, ctx, pool, cidA)
	require.False(t, stA.LocalPresent, "(a) reconcile: local_present must be false after reconcile")
	require.Equal(t, gen.CoordinatorLocalRoleAbsent, stA.LocalRole, "(a) reconcile: role must be absent")

	// (b): reconcile absent-projection / backend-present ⇒ backend unpinned,
	// projection remains absent (reclaim orphaned pin, do not re-adopt).
	err = p.reconcilePresence(ctx, cidB)
	require.NoError(t, err)
	require.False(t, be.has(cidB), "(b) reconcile: orphaned backend pin must be reclaimed (Unpin)")
	stB := getState(t, ctx, pool, cidB)
	require.False(t, stB.LocalPresent, "(b) reconcile: projection still absent after orphan reclaim")
	require.Equal(t, gen.CoordinatorLocalRoleAbsent, stB.LocalRole, "(b) reconcile: role still absent")
}

// TestBoundedCacheEvictsHolderSafeOnly: in bounded_cache mode with the cache
// over BoundedCacheMaxBytes, pruneOnce triggers cache.evictToFit so cache-role
// rows are evicted. Seeds cache rows directly into blob_storage_state (bypassing
// the auto-evict path in cachePolicy.admit) so we can force an over-budget state,
// then assert pruneOnce restores the budget.
func TestBoundedCacheEvictsHolderSafeOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	// Very small budget: 100 bytes. We'll seed 3 × 100-byte cache objects directly
	// (bypassing cachePolicy.admit which would already evict inline).
	cp := newCachePolicy(pool, be, StorageModeConfig{
		Mode: StorageModeBoundedCache, MaxBytes: 100, ProtectedRatio: 0.8,
		TouchInterval: time.Minute,
	})

	// Directly seed cache-role rows so we can force over-budget state.
	cids := make([]string, 3)
	for i, label := range []string{"bc-ev1", "bc-ev2", "bc-ev3"} {
		c := cacheCID(t, ctx, pool, be, label, 100)
		cids[i] = c
		// Insert directly as a committed cache row (bypassing admit's inline eviction).
		_, err := pool.Exec(ctx, `
			INSERT INTO blob_storage_state
				(cid, commit_state, durability_class, local_role, cache_segment, local_present, local_bytes, updated_at)
			VALUES ($1, 'committed', 'cache', 'cache', 'probationary', true, 100, now())
			ON CONFLICT (cid) DO UPDATE SET
				commit_state='committed', durability_class='cache',
				local_role='cache', cache_segment='probationary',
				local_present=true, local_bytes=100, updated_at=now()
		`, c)
		require.NoError(t, err)
		// Backdate so eviction order is deterministic.
		_, err = pool.Exec(ctx,
			`UPDATE blob_storage_state SET last_accessed_at = now() - make_interval(secs => $2) WHERE cid=$1`,
			c, float64(30-i))
		require.NoError(t, err)
	}

	// Verify we're over budget before pruneOnce.
	sums, err := gen.New(pool).SumCacheBytes(ctx)
	require.NoError(t, err)
	require.Greater(t, sums.ProbationaryBytes+sums.ProtectedBytes, int64(100),
		"setup: total cache must exceed MaxBytes before pruneOnce")

	// There are no origin blobs (important/normal), so pruneOnce will only trigger
	// cache eviction via evictToFit after iterating (empty) origin classes.
	p := newPrunerForTest(pool, be, cp, 2, 3600, 60*time.Second)
	p.pruneOnce(ctx)

	// After pruneOnce, total cache bytes must be within budget.
	sums, err = gen.New(pool).SumCacheBytes(ctx)
	require.NoError(t, err)
	require.LessOrEqual(t, sums.ProbationaryBytes+sums.ProtectedBytes, int64(100),
		"after pruneOnce in bounded_cache mode, cache must be within MaxBytes budget")

	// At least one cache row must have been evicted (local_present=false).
	evicted := 0
	for _, c := range cids {
		st := getState(t, ctx, pool, c)
		if !st.LocalPresent {
			evicted++
		}
	}
	require.GreaterOrEqual(t, evicted, 2, "at least 2 cache rows must be evicted to fit the 100-byte budget")
}

// TestNewPrunerNilOnOriginCopy verifies NewPruner returns nil when the storage
// mode is origin_copy (origin pruning is disabled — never evict from origin).
func TestNewPrunerNilOnOriginCopy(t *testing.T) {
	// This is a pure-logic test — no DB needed.
	svc := &Service{cache: newCachePolicyFor(nil, newEchoBackend(), StorageModeConfig{
		Mode: StorageModeOriginCopy,
	})}
	p := NewPruner(svc, PrunerConfig{Floor: 2, StaleSeconds: 3600, Interval: 60 * time.Second})
	require.Nil(t, p, "NewPruner must return nil in origin_copy mode")
}

// TestNewPrunerNilOnNilCache verifies NewPruner returns nil when Service.cache is nil.
func TestNewPrunerNilOnNilCache(t *testing.T) {
	svc := &Service{cache: nil}
	p := NewPruner(svc, PrunerConfig{Floor: 2, StaleSeconds: 3600, Interval: 60 * time.Second})
	require.Nil(t, p, "NewPruner must return nil when cache is nil (no cache policy installed)")
}

// TestPrunerAlsoPrunesNormalClass verifies that pruneOnce handles the "normal"
// durability class (derivatives), not only "important".
func TestPrunerAlsoPrunesNormalClass(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	payload := []byte("pruner-normal-class-test")
	cidStr := mkRawCID(t, payload)
	const envSize = 200

	// Use "normal" (derivative) class.
	seedOriginBlob(t, ctx, pool, be, cidStr, envSize, "normal")

	// Seed floor=2 acked holders.
	n1, n2 := uuid.New(), uuid.New()
	seedSourceableNode(t, ctx, pool, n1)
	seedSourceableNode(t, ctx, pool, n2)
	seedAckedHolder(t, ctx, pool, cidStr, n1)
	seedAckedHolder(t, ctx, pool, cidStr, n2)

	p := newPrunerForTest(pool, be, nil, 2, 3600, 60*time.Second)
	p.pruneOnce(ctx)

	st := getState(t, ctx, pool, cidStr)
	require.False(t, st.LocalPresent, "normal-class blob must also be pruned once floor is met")
	require.False(t, be.has(cidStr), "normal-class blob must be unpinned")
}

// TestPrunerSkipsCacheClass ensures pruneOnce never prunes 'cache'-class rows
// (that's Task 9's size eviction, not origin pruning).
func TestPrunerSkipsCacheClass(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	payload := []byte("pruner-cache-class-skip-test")
	cidStr := mkRawCID(t, payload)
	const envSize = 64

	// Seed as a cache-class committed local_present row (mimics AdmitToCache path).
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ($1, 'application/octet-stream', $2, 'active', 'raw', 2)
		ON CONFLICT (cid) DO NOTHING
	`, cidStr, envSize)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_storage_state
			(cid, commit_state, durability_class, local_role, local_present, local_bytes, updated_at)
		VALUES ($1, 'committed', 'cache', 'cache', true, $2, now())
		ON CONFLICT (cid) DO UPDATE SET
			commit_state = 'committed', durability_class = 'cache',
			local_role = 'cache', local_present = true, local_bytes = EXCLUDED.local_bytes,
			updated_at = now()
	`, cidStr, envSize)
	require.NoError(t, err)
	be.put(cidStr, payload)

	// Seed >floor holders (doesn't matter — the pruner shouldn't touch cache class).
	n1, n2 := uuid.New(), uuid.New()
	seedSourceableNode(t, ctx, pool, n1)
	seedSourceableNode(t, ctx, pool, n2)
	seedAckedHolder(t, ctx, pool, cidStr, n1)
	seedAckedHolder(t, ctx, pool, cidStr, n2)

	p := newPrunerForTest(pool, be, nil, 2, 3600, 60*time.Second)
	p.pruneOnce(ctx)

	// The cache row must be untouched (origin pruner only targets important/normal).
	st, serr := gen.New(pool).GetStorageState(ctx, cidStr)
	require.NoError(t, serr)
	require.True(t, st.LocalPresent, "cache-class row must NOT be pruned by the origin pruner")
	require.True(t, be.has(cidStr), "cache-class blob must remain in backend")
}
