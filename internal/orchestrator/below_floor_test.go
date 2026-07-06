package orchestrator

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// P2-M7.1 below-floor query classification (D-M7.1-3), mirroring the drain
// classification (D-M7-6b/6c): safety counts EXCLUDE a SUSTAINED-below-floor
// node (marker older than grace) while an in-grace marker still counts;
// placement never targets a sustained node; read/repair source SELECTION may
// still use below-floor nodes, deprioritized after the drain sort key (bare
// marker — even an in-grace node is slightly deprioritized as a source; that
// is intended). One fixture throughout: CID "bf-cid" with target_count=2,
// acked on nodes A (to sink below floor) and B (healthy); node C is an
// eligible empty destination.

const (
	bfNodeA = "bbbbbbbb-bbbb-bbbb-bbbb-000000000001"
	bfNodeB = "bbbbbbbb-bbbb-bbbb-bbbb-000000000002"
	bfNodeC = "bbbbbbbb-bbbb-bbbb-bbbb-000000000003"

	// bfGraceSecs is the test grace window (1h). "Sustained" markers are set
	// 2h in the past; "in-grace" markers 1min in the past.
	bfGraceSecs     = 3600.0
	bfSustainedSecs = 7200.0
	bfInGraceSecs   = 60.0
)

// seedBelowFloorFixture: blob bf-cid (target 2), A+B acked holders, C empty
// eligible destination; all three live/current/read-sourceable and fresh.
func seedBelowFloorFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	seedBlob(t, ctx, pool, "bf-cid", "normal", true)
	for _, n := range []string{bfNodeA, bfNodeB, bfNodeC} {
		seedNode(t, ctx, pool, n, "active", "current", true)
	}
	_, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at = now()`)
	require.NoError(t, err)
	assignPinState(t, ctx, pool, "bf-cid", bfNodeA, "acked")
	assignPinState(t, ctx, pool, "bf-cid", bfNodeB, "acked")
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_replication_state
			(cid, healthy_acked_count, sourceable_acked_count, in_flight_count,
			 target_count, safety_tier, local_recoverable, durability_class, dirty)
		VALUES ('bf-cid', 2, 2, 0, 2, 'healthy', true, 'normal', false)`)
	require.NoError(t, err)
}

// markBelowFloor backdates the node's below_floor_since by ageSecs. The Task 11
// sweep owns the marker in production; tests write it directly (schema-level).
func markBelowFloor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, ageSecs float64) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		UPDATE nodes SET below_floor_since = now() - make_interval(secs => $2::float)
		WHERE id = $1::uuid`, id, ageSecs)
	require.NoError(t, err)
}

func TestBelowFloorSustainedExcludedFromSafetyCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedBelowFloorFixture(t, ctx, pool)
	q := gen.New(pool)

	before, err := q.RecomputeReplicationCounts(ctx, gen.RecomputeReplicationCountsParams{
		Cid: "bf-cid", BelowFloorGraceSecs: bfGraceSecs,
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, before.HealthyAcked)
	require.EqualValues(t, 2, before.SourceableAcked)

	markBelowFloor(t, ctx, pool, bfNodeA, bfSustainedSecs)

	after, err := q.RecomputeReplicationCounts(ctx, gen.RecomputeReplicationCountsParams{
		Cid: "bf-cid", BelowFloorGraceSecs: bfGraceSecs,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, after.HealthyAcked, "sustained-below-floor A must not be durability-countable (D-M7.1-3)")
	require.EqualValues(t, 1, after.SourceableAcked, "sustained-below-floor A must not be safety-sourceable (D-M7.1-3)")

	// The commit/prune/read safety count (storage_state.sql) must agree.
	n, err := q.CountSourceableHolders(ctx, gen.CountSourceableHoldersParams{
		Cid: "bf-cid", StaleSecs: 3600, BelowFloorGraceSecs: bfGraceSecs,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "CountSourceableHolders must exclude sustained-below-floor A")
}

func TestBelowFloorInGraceStillCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedBelowFloorFixture(t, ctx, pool)
	q := gen.New(pool)

	// A dipped below the floor a minute ago — inside the grace window, so
	// reputation wobble near the floor must not flap the safety counts.
	markBelowFloor(t, ctx, pool, bfNodeA, bfInGraceSecs)

	counts, err := q.RecomputeReplicationCounts(ctx, gen.RecomputeReplicationCountsParams{
		Cid: "bf-cid", BelowFloorGraceSecs: bfGraceSecs,
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, counts.HealthyAcked, "in-grace below-floor A still counts (hysteresis, D-M7.1-3)")
	require.EqualValues(t, 2, counts.SourceableAcked)

	n, err := q.CountSourceableHolders(ctx, gen.CountSourceableHoldersParams{
		Cid: "bf-cid", StaleSecs: 3600, BelowFloorGraceSecs: bfGraceSecs,
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, n, "in-grace below-floor A still counts for commit/prune safety")
}

func TestBelowFloorSustainedExcludedFromPlacement(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedBelowFloorFixture(t, ctx, pool)
	q := gen.New(pool)
	markBelowFloor(t, ctx, pool, bfNodeA, bfSustainedSecs)

	// A CID nobody holds: A/B/C are all non-holders, but A is sustained.
	rows, err := q.ListPlacementCandidates(ctx, gen.ListPlacementCandidatesParams{
		Cid: "other-cid", BelowFloorGraceSecs: bfGraceSecs,
	})
	require.NoError(t, err)
	got := map[string]bool{}
	for _, r := range rows {
		got[uuidString(r.NodeID)] = true
	}
	require.False(t, got[bfNodeA], "sustained-below-floor node must never be a placement destination (D-M7.1-3)")
	require.True(t, got[bfNodeC], "eligible empty node stays a destination")
	require.True(t, got[bfNodeB])

	// In-grace: A returns to the candidate set (grace applies to placement too).
	markBelowFloor(t, ctx, pool, bfNodeA, bfInGraceSecs)
	rows, err = q.ListPlacementCandidates(ctx, gen.ListPlacementCandidatesParams{
		Cid: "other-cid", BelowFloorGraceSecs: bfGraceSecs,
	})
	require.NoError(t, err)
	got = map[string]bool{}
	for _, r := range rows {
		got[uuidString(r.NodeID)] = true
	}
	require.True(t, got[bfNodeA], "in-grace below-floor node remains a placement candidate")
}

func TestBelowFloorDeprioritizedNotExcludedFromSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedBelowFloorFixture(t, ctx, pool)
	q := gen.New(pool)

	// A outranks B on reputation — the below-floor sort key must still win.
	// NOTE: bare `below_floor_since IS NOT NULL` (no grace) in selection —
	// even an IN-GRACE below-floor node is slightly deprioritized (intended).
	_, err := pool.Exec(ctx, `UPDATE nodes SET reputation_score = 0.9 WHERE id = $1::uuid`, bfNodeA)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE nodes SET reputation_score = 0.5 WHERE id = $1::uuid`, bfNodeB)
	require.NoError(t, err)
	markBelowFloor(t, ctx, pool, bfNodeA, bfInGraceSecs)

	holders, err := q.ListSourceableHolders(ctx, gen.ListSourceableHoldersParams{
		Cid: "bf-cid", StaleSecs: 3600,
	})
	require.NoError(t, err)
	require.Len(t, holders, 2, "below-floor A stays selectable as a read source")
	require.Equal(t, bfNodeB, uuidString(holders[0].NodeID), "healthy B sorts first despite lower reputation")
	require.Equal(t, bfNodeA, uuidString(holders[1].NodeID), "below-floor A sorts last")

	// Repair-source selection: B wins while both hold the CID, despite A's
	// higher reputation × capacity weight.
	_, err = pool.Exec(ctx, `
		UPDATE nodes SET advertised_capabilities = '{read-source/v1,repair-stream/v1}'
		WHERE id = ANY(ARRAY[$1, $2]::uuid[])`, bfNodeA, bfNodeB)
	require.NoError(t, err)
	src, err := q.ListRepairSourceHolders(ctx, gen.ListRepairSourceHoldersParams{
		Cid: "bf-cid", Size: pgtype.Int8{Int64: 100, Valid: true},
	})
	require.NoError(t, err)
	require.Equal(t, bfNodeB, uuidString(src.NodeID),
		"healthy B outranks below-floor A as repair source (D-M7.1-3)")

	// Repair source of last resort: a CID whose ONLY acked holder is
	// below-floor A (same principle as drain's self-sourcing).
	seedBlob(t, ctx, pool, "bf-repair-cid", "normal", true)
	assignPinState(t, ctx, pool, "bf-repair-cid", bfNodeA, "acked")
	src, err = q.ListRepairSourceHolders(ctx, gen.ListRepairSourceHoldersParams{
		Cid: "bf-repair-cid", Size: pgtype.Int8{Int64: 100, Valid: true},
	})
	require.NoError(t, err)
	require.Equal(t, bfNodeA, uuidString(src.NodeID),
		"a below-floor node remains a repair source of last resort (D-M7.1-3)")
}

// TestBelowFloorAndDrainSortKeyOrder pins the prepended sort-key order: the
// drain key comes FIRST, below-floor second — a below-floor-but-live node is
// preferred over a draining one, and a healthy node over both.
func TestBelowFloorAndDrainSortKeyOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedBelowFloorFixture(t, ctx, pool)
	q := gen.New(pool)

	// C also holds bf-cid: A below-floor, B draining, C healthy.
	assignPinState(t, ctx, pool, "bf-cid", bfNodeC, "acked")
	markBelowFloor(t, ctx, pool, bfNodeA, bfSustainedSecs)
	nDrain, err := gen.New(pool).SetNodeDraining(ctx, pgUUID(t, bfNodeB))
	require.NoError(t, err)
	require.EqualValues(t, 1, nDrain)

	holders, err := q.ListSourceableHolders(ctx, gen.ListSourceableHoldersParams{
		Cid: "bf-cid", StaleSecs: 3600,
	})
	require.NoError(t, err)
	require.Len(t, holders, 3)
	require.Equal(t, bfNodeC, uuidString(holders[0].NodeID), "healthy C first")
	require.Equal(t, bfNodeA, uuidString(holders[1].NodeID), "below-floor A before draining B")
	require.Equal(t, bfNodeB, uuidString(holders[2].NodeID), "draining B last")
}
