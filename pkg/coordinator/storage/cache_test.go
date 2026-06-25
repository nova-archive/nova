package storage

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	gocid "github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// seedBlobForCache inserts a blobs row so the blob_storage_state FK (cid →
// blobs.cid) is satisfied. The cache tests admit/evict against these rows.
func seedBlobForCache(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string, bytes int64) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ($1, 'application/octet-stream', $2, 'active', 'raw', 2)
		ON CONFLICT (cid) DO NOTHING
	`, cid, bytes)
	require.NoError(t, err)
}

// newCachePolicy builds a cachePolicy over a real *gen.Queries and the given
// mode config, for the DB-backed SLRU tests.
func newCachePolicy(pool *pgxpool.Pool, be *echoBackend, cfg StorageModeConfig) *cachePolicy {
	return newCachePolicyFor(gen.New(pool), be, cfg)
}

// getState reads the storage-state row for assertions.
func getState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string) gen.BlobStorageState {
	t.Helper()
	st, err := gen.New(pool).GetStorageState(ctx, cid)
	require.NoError(t, err)
	return st
}

// cacheCID derives a REAL, decodable CIDv1 from a label (so cachePolicy.evict's
// gocid.Decode + backend.Unpin actually run — fake "cid-foo" strings would make
// the unpin a silent no-op and mask eviction bugs), seeds the blobs FK row and
// the backend bytes, and returns the CID string. envSize is the local_bytes the
// test admits with (it need not equal len(payload); byte accounting is by
// envSize, D-M4.1-16).
func cacheCID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, be *echoBackend, label string, envSize int64) string {
	t.Helper()
	payload := []byte("nova-cache-test:" + label)
	cidStr := mkRawCID(t, payload)
	seedBlobForCache(t, ctx, pool, cidStr, envSize)
	be.put(cidStr, payload)
	return cidStr
}

func TestModeOriginCopyKeeps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	cp := newCachePolicy(pool, be, StorageModeConfig{Mode: StorageModeOriginCopy, MaxBytes: 100})

	// Admit several oversize objects; origin_copy must never evict.
	for _, c := range []string{"cid-oc1", "cid-oc2", "cid-oc3"} {
		seedBlobForCache(t, ctx, pool, c, 100)
		cp.admit(ctx, c, 100)
	}

	for _, c := range []string{"cid-oc1", "cid-oc2", "cid-oc3"} {
		st := getState(t, ctx, pool, c)
		require.True(t, st.LocalPresent, "origin_copy must not evict %s", c)
		require.Equal(t, gen.CoordinatorLocalRoleCache, st.LocalRole)
	}
}

func TestSLRUAdmitProbationaryThenPromoteOnHit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	cp := newCachePolicy(pool, be, StorageModeConfig{
		Mode: StorageModeBoundedCache, MaxBytes: 1000, ProtectedRatio: 0.8,
		TouchInterval: time.Minute,
	})

	const c = "cid-promote"
	seedBlobForCache(t, ctx, pool, c, 100)

	// First fetch → probationary.
	cp.admit(ctx, c, 100)
	st := getState(t, ctx, pool, c)
	require.True(t, st.CacheSegment.Valid)
	require.Equal(t, gen.CacheSegmentProbationary, st.CacheSegment.CacheSegment,
		"first admission lands in probationary")

	// Backdate last_accessed_at so the throttle does not suppress the promote.
	_, err := pool.Exec(ctx, `UPDATE blob_storage_state SET last_accessed_at = now() - interval '1 hour' WHERE cid=$1`, c)
	require.NoError(t, err)

	// Second access (cache hit) → protected.
	cp.onHit(ctx, c)
	st = getState(t, ctx, pool, c)
	require.True(t, st.CacheSegment.Valid)
	require.Equal(t, gen.CacheSegmentProtected, st.CacheSegment.CacheSegment,
		"a cache hit promotes a probationary object to protected")
}

func TestSLRUEvictsProbationaryBeforeProtected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	// Budget = 300 bytes; each object = 100 bytes, so 3 fit. ProtectedRatio high
	// enough that one protected object does not trip the ratio cap.
	cp := newCachePolicy(pool, be, StorageModeConfig{
		Mode: StorageModeBoundedCache, MaxBytes: 300, ProtectedRatio: 0.8,
		TouchInterval: time.Minute,
	})

	// "hot" is fetched then hit twice → protected; it must survive the scan.
	hot := cacheCID(t, ctx, pool, be, "hot", 100)
	cp.admit(ctx, hot, 100)
	_, err := pool.Exec(ctx, `UPDATE blob_storage_state SET last_accessed_at = now() - interval '1 hour' WHERE cid=$1`, hot)
	require.NoError(t, err)
	cp.onHit(ctx, hot) // promote to protected
	require.Equal(t, gen.CacheSegmentProtected, getState(t, ctx, pool, hot).CacheSegment.CacheSegment)

	// A one-off cold scan of many probationary objects. Each is admitted once
	// (never hit), so they stay probationary and drain first under pressure.
	cold := make([]string, 0, 5)
	for i, label := range []string{"cold1", "cold2", "cold3", "cold4", "cold5"} {
		c := cacheCID(t, ctx, pool, be, label, 100)
		cold = append(cold, c)
		cp.admit(ctx, c, 100)
		// Backdate so eviction order is deterministic (older cold evicted first).
		_, err := pool.Exec(ctx,
			`UPDATE blob_storage_state SET last_accessed_at = now() - make_interval(secs => $2) WHERE cid=$1`,
			c, float64(100-i))
		require.NoError(t, err)
	}

	// Total budget respected: probationary + protected ≤ 300.
	sums, err := gen.New(pool).SumCacheBytes(ctx)
	require.NoError(t, err)
	require.LessOrEqual(t, sums.ProbationaryBytes+sums.ProtectedBytes, int64(300),
		"total cached bytes must stay within MaxBytes")

	// The protected hot object must survive the cold scan.
	hotSt := getState(t, ctx, pool, hot)
	require.True(t, hotSt.LocalPresent, "twice-hit protected object must survive a cold scan")
	require.Equal(t, gen.CacheSegmentProtected, hotSt.CacheSegment.CacheSegment)

	// At least one cold (probationary) object must have been evicted (absent +
	// unpinned). With 1 protected + 5 cold admitted into a 3-slot budget, the
	// cold set is the eviction victim.
	evicted := 0
	for _, c := range cold {
		st := getState(t, ctx, pool, c)
		if !st.LocalPresent {
			evicted++
			require.Equal(t, gen.CoordinatorLocalRoleAbsent, st.LocalRole, "evicted row role=absent")
			require.False(t, st.CacheSegment.Valid, "evicted row cache_segment=NULL")
			require.False(t, be.has(c), "evicted cid must be unpinned in the backend")
		}
	}
	require.GreaterOrEqual(t, evicted, 2, "cold probationary objects must be evicted before the protected hot object")
}

func TestProtectedRatioCapDemotes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	// MaxBytes 1000, ProtectedRatio 0.5 → protected cap = 500 bytes (5 objects).
	cp := newCachePolicy(pool, be, StorageModeConfig{
		Mode: StorageModeBoundedCache, MaxBytes: 1000, ProtectedRatio: 0.5,
		TouchInterval: time.Minute,
	})

	// Promote 6 objects to protected (600 bytes > 500 cap). The oldest must be
	// evicted (unpinned + absent) to bring protected back within the ratio cap.
	labels := []string{"p1", "p2", "p3", "p4", "p5", "p6"}
	cids := make([]string, 0, len(labels))
	for i, label := range labels {
		c := cacheCID(t, ctx, pool, be, label, 100)
		cids = append(cids, c)
		cp.admit(ctx, c, 100)
		// Force protected directly, with a distinct (increasing) last_accessed_at
		// so p1 is oldest.
		_, err := pool.Exec(ctx, `
			UPDATE blob_storage_state
			SET cache_segment='protected', last_accessed_at = now() - make_interval(secs => $2)
			WHERE cid=$1
		`, c, float64(len(labels)-i))
		require.NoError(t, err)
	}
	oldest := cids[0] // p1

	// Trigger evictToFit by admitting one more object (probationary).
	trigger := cacheCID(t, ctx, pool, be, "trigger", 50)
	cp.admit(ctx, trigger, 50)

	// The oldest protected entry (p1) must be gone.
	st := getState(t, ctx, pool, oldest)
	require.False(t, st.LocalPresent, "oldest protected entry must be evicted when protected exceeds the ratio cap")
	require.Equal(t, gen.CoordinatorLocalRoleAbsent, st.LocalRole)
	require.False(t, be.has(oldest), "demoted/evicted oldest protected must be unpinned")

	// Protected total must now be within the cap.
	sums, err := gen.New(pool).SumCacheBytes(ctx)
	require.NoError(t, err)
	require.LessOrEqual(t, sums.ProtectedBytes, int64(500), "protected bytes must be within the ratio cap")
}

func TestMaxObjectBytesRefusesAdmission(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	cp := newCachePolicy(pool, be, StorageModeConfig{
		Mode: StorageModeBoundedCache, MaxBytes: 10000, ProtectedRatio: 0.8,
		MaxObjectBytes: 100, TouchInterval: time.Minute,
	})

	// Under the ceiling → admitted.
	seedBlobForCache(t, ctx, pool, "cid-small", 50)
	cp.admit(ctx, "cid-small", 50)
	st, err := gen.New(pool).GetStorageState(ctx, "cid-small")
	require.NoError(t, err)
	require.True(t, st.LocalPresent, "object under the ceiling must be admitted")

	// Over the ceiling → refused (no row created).
	seedBlobForCache(t, ctx, pool, "cid-big", 500)
	cp.admit(ctx, "cid-big", 500)
	_, err = gen.New(pool).GetStorageState(ctx, "cid-big")
	require.Error(t, err, "object over MaxObjectBytes must not be admitted (no cache row)")
}

func TestCacheTouchAndPromoteThrottled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	be := newEchoBackend()

	cp := newCachePolicy(pool, be, StorageModeConfig{
		Mode: StorageModeBoundedCache, MaxBytes: 1000, ProtectedRatio: 0.8,
		TouchInterval: time.Hour, // long interval ⇒ promotes/touches are throttled
	})

	const c = "cid-throttle"
	seedBlobForCache(t, ctx, pool, c, 100)
	cp.admit(ctx, c, 100) // last_accessed_at = now(), probationary

	// onHit within the throttle interval must NOT promote (last access is recent).
	cp.onHit(ctx, c)
	st := getState(t, ctx, pool, c)
	require.Equal(t, gen.CacheSegmentProbationary, st.CacheSegment.CacheSegment,
		"a hot read within the throttle interval must not promote (no DB churn)")

	// Backdate beyond the interval; now onHit promotes.
	_, err := pool.Exec(ctx, `UPDATE blob_storage_state SET last_accessed_at = now() - interval '2 hours' WHERE cid=$1`, c)
	require.NoError(t, err)
	cp.onHit(ctx, c)
	st = getState(t, ctx, pool, c)
	require.Equal(t, gen.CacheSegmentProtected, st.CacheSegment.CacheSegment,
		"once past the throttle interval, a hit promotes to protected")
}

// TestTransientUnpinsAfterClose exercises the public_archival (unencrypted,
// streamed) transient path: the pin is present while the reader is open and
// gone after Close (via the unpinOnClose wrapper). It uses the in-memory
// echoBackend — this tests wrapper/unpin timing, not SQL.
func TestTransientUnpinsAfterClose(t *testing.T) {
	ctx := context.Background()
	data := []byte("transient public_archival bytes streamed from a donor")
	cidStr := mkRawCID(t, data)

	be := newEchoBackend() // local MISS

	fetch := &fakeFetcher{byAddr: map[string][]byte{"donor-a:4242": data}}
	q := &fakeQuerier{envSize: int64(len(data)), holders: []gen.ListSourceableHoldersRow{holderRow("donor-a:4242", 0.9)}}
	svc := &Service{backend: be}
	svc.setDonorReadSourceForTest(fetch, newTestSigner(t), q, time.Minute, 86400)
	// Install a transient-mode cache policy (no DB queries are issued in transient
	// mode beyond what admit would do — transient holds nothing).
	svc.cache = newCachePolicyFor(nil, be, StorageModeConfig{Mode: StorageModeTransient})

	rc, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data))))
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}

	// While the reader is open, the donor-fetched bytes are pinned locally.
	c, _ := gocid.Decode(cidStr)
	has, _ := be.Has(ctx, c)
	require.True(t, has, "pin must be present while the public_archival reader is open")

	got, _ := io.ReadAll(rc)
	require.True(t, bytes.Equal(got, data), "served bytes must match")

	// Closing the reader must unpin (transient: the next read is donor-backed).
	require.NoError(t, rc.Close())
	has, _ = be.Has(ctx, c)
	require.False(t, has, "pin must be gone after Close in transient mode")
}
