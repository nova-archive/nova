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

	// bfDistinctStaleSecs is the CountSourceableHolders freshness window used
	// by these tests: deliberately DISTINCT from bfGraceSecs AND larger than
	// bfSustainedSecs, so a transposed stale/grace argument pair cannot pass
	// (all fixture nodes are fresh, so any positive stale window is inert).
	bfDistinctStaleSecs = 86400.0
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
	// StaleSecs is deliberately DISTINCT from the grace (transposition-proof:
	// a swapped stale/grace argument would read the 2h-old marker as in-grace
	// and flip this assertion).
	n, err := q.CountSourceableHolders(ctx, gen.CountSourceableHoldersParams{
		Cid: "bf-cid", StaleSecs: bfDistinctStaleSecs, BelowFloorGraceSecs: bfGraceSecs,
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
		Cid: "bf-cid", StaleSecs: bfDistinctStaleSecs, BelowFloorGraceSecs: bfGraceSecs,
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
		UPDATE nodes SET advertised_capabilities = '{read-source/v1,repair-stream/v1,blob-transfer/v1}', effective_capabilities = '{read-source/v1,repair-stream/v1,blob-transfer/v1}'
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
// below-floor key comes FIRST, drain second — the composite preference is
// healthy > draining > below-floor (P2-M7.1 review decision; the design prose
// governs). A draining node is trusted data leaving politely; a below-floor
// node is DISTRUSTED — it is the TRUE last resort.
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
	require.Equal(t, bfNodeB, uuidString(holders[1].NodeID), "draining B before below-floor A")
	require.Equal(t, bfNodeA, uuidString(holders[2].NodeID), "below-floor A last (true last resort)")

	// The repair-source selection must agree: draining B outranks below-floor A.
	_, err = pool.Exec(ctx, `
		UPDATE nodes SET advertised_capabilities = '{read-source/v1,repair-stream/v1,blob-transfer/v1}', effective_capabilities = '{read-source/v1,repair-stream/v1,blob-transfer/v1}'
		WHERE id = ANY(ARRAY[$1, $2]::uuid[])`, bfNodeA, bfNodeB)
	require.NoError(t, err)
	seedBlob(t, ctx, pool, "bf-order-cid", "normal", true)
	assignPinState(t, ctx, pool, "bf-order-cid", bfNodeA, "acked")
	assignPinState(t, ctx, pool, "bf-order-cid", bfNodeB, "acked")
	src, err := q.ListRepairSourceHolders(ctx, gen.ListRepairSourceHoldersParams{
		Cid: "bf-order-cid", Size: pgtype.Int8{Int64: 100, Valid: true},
	})
	require.NoError(t, err)
	require.Equal(t, bfNodeB, uuidString(src.NodeID),
		"draining B outranks below-floor A as repair source (healthy > draining > below-floor)")
}

// ---------------------------------------------------------------------------
// Task 11 sweep WRITE-side (D-M7.1-3): marker maintenance, requeue, demote.
// ---------------------------------------------------------------------------

// bfSweepCfg builds an enabled BelowFloorConfig for the sweep functions.
func bfSweepCfg(floor, margin, graceSecs float64, batch int) BelowFloorConfig {
	return BelowFloorConfig{
		Enabled: true, ReputationFloor: floor, HysteresisMargin: margin,
		GraceSeconds: graceSecs, RequeueBatch: batch,
	}
}

func setReputation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, score float64) {
	t.Helper()
	_, err := pool.Exec(ctx, `UPDATE nodes SET reputation_score = $2 WHERE id = $1::uuid`, id, score)
	require.NoError(t, err)
}

func belowFloorMarker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) pgtype.Timestamptz {
	t.Helper()
	var ts pgtype.Timestamptz
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT below_floor_since FROM nodes WHERE id = $1::uuid`, id).Scan(&ts))
	return ts
}

func assertPinState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid, node, want string) {
	t.Helper()
	var state string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT state FROM pin_assignments WHERE cid=$1 AND node_id=$2::uuid`, cid, node).Scan(&state))
	require.Equal(t, want, state)
}

// TestBelowFloorMarkerHysteresis: the marker is stamped once reputation sinks
// below the floor, is NEVER re-stamped or cleared by wobble inside the
// hysteresis band [floor, floor+margin), and clears only on recovery past
// floor+margin (contract item 1).
func TestBelowFloorMarkerHysteresis(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedNode(t, ctx, pool, bfNodeA, "active", "current", true)
	cfg := bfSweepCfg(0.5, 0.1, bfGraceSecs, 500)

	// Above the floor → no marker.
	setReputation(t, ctx, pool, bfNodeA, 0.8)
	res, err := MaintainBelowFloorMarkers(ctx, pool, cfg)
	require.NoError(t, err)
	require.Equal(t, 0, res.Marked)
	require.False(t, belowFloorMarker(t, ctx, pool, bfNodeA).Valid, "healthy reputation is not marked")

	// Below the floor → marker set.
	setReputation(t, ctx, pool, bfNodeA, 0.4)
	res, err = MaintainBelowFloorMarkers(ctx, pool, cfg)
	require.NoError(t, err)
	require.Equal(t, 1, res.Marked)
	first := belowFloorMarker(t, ctx, pool, bfNodeA)
	require.True(t, first.Valid, "sub-floor reputation is marked")

	// Wobble UP into the band [0.5, 0.6) → not cleared, not re-stamped.
	setReputation(t, ctx, pool, bfNodeA, 0.55)
	res, err = MaintainBelowFloorMarkers(ctx, pool, cfg)
	require.NoError(t, err)
	require.Equal(t, 0, res.Marked, "an already-marked node is never re-stamped")
	require.Equal(t, 0, res.Cleared, "wobble inside the band does not clear")
	require.Equal(t, first.Time, belowFloorMarker(t, ctx, pool, bfNodeA).Time,
		"the sustainment clock is not reset by band wobble")

	// Recover past floor+margin → cleared.
	setReputation(t, ctx, pool, bfNodeA, 0.65)
	res, err = MaintainBelowFloorMarkers(ctx, pool, cfg)
	require.NoError(t, err)
	require.Equal(t, 1, res.Cleared)
	require.False(t, belowFloorMarker(t, ctx, pool, bfNodeA).Valid,
		"recovery past floor+margin clears the marker")
}

// TestBelowFloorMarkerRunsWhenRemedyDisabled: marker maintenance is
// observability and runs regardless of Enabled — only the requeue/demote remedy
// is gated (contract item 5; the gate itself lives in Orchestrator.runOnce).
func TestBelowFloorMarkerRunsWhenRemedyDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedNode(t, ctx, pool, bfNodeA, "active", "current", true)
	cfg := bfSweepCfg(0.5, 0.1, bfGraceSecs, 500)
	cfg.Enabled = false

	setReputation(t, ctx, pool, bfNodeA, 0.4)
	res, err := MaintainBelowFloorMarkers(ctx, pool, cfg)
	require.NoError(t, err)
	require.Equal(t, 1, res.Marked, "marker maintenance runs with the remedy disabled (gauge stays truthful)")
	require.True(t, belowFloorMarker(t, ctx, pool, bfNodeA).Valid)
}

// TestBelowFloorRequeueGraceAndBatch: only SUSTAINED nodes' CIDs are requeued,
// with reason 'below_floor', capped at requeue_batch (contracts 2 + 3).
func TestBelowFloorRequeueGraceAndBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedNode(t, ctx, pool, bfNodeA, "active", "current", true)
	for _, c := range []string{"rq-1", "rq-2", "rq-3"} {
		seedBlob(t, ctx, pool, c, "normal", true)
		assignPinState(t, ctx, pool, c, bfNodeA, "acked")
	}
	cfg := bfSweepCfg(0.5, 0.1, bfGraceSecs, 2) // batch cap 2

	// In-grace → no replacement traffic.
	markBelowFloor(t, ctx, pool, bfNodeA, bfInGraceSecs)
	res, err := ReplaceBelowFloor(ctx, pool, cfg)
	require.NoError(t, err)
	require.Equal(t, 0, res.Requeued, "an in-grace node receives no replacement traffic")

	// Sustained → requeue, capped at batch=2.
	markBelowFloor(t, ctx, pool, bfNodeA, bfSustainedSecs)
	res, err = ReplaceBelowFloor(ctx, pool, cfg)
	require.NoError(t, err)
	require.Equal(t, 2, res.Requeued, "requeue is capped at requeue_batch across all sustained nodes")

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blob_replication_reconcile_queue WHERE reason='below_floor'`).Scan(&n))
	require.Equal(t, 2, n, "requeued CIDs carry reason 'below_floor'")
}

// TestBelowFloorReplaceThenDemote: a sustained node's replica is failed with
// state='failed' ONLY once a trusted replacement restores the healthy count —
// never a durability dip (contract item 4).
func TestBelowFloorReplaceThenDemote(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedBelowFloorFixture(t, ctx, pool) // bf-cid target 2, A+B acked, brs healthy=2
	cfg := bfSweepCfg(0.5, 0.1, bfGraceSecs, 500)
	markBelowFloor(t, ctx, pool, bfNodeA, bfSustainedSecs)

	// Only A (sustained) + B hold bf-cid; excluding A, live holders = 1 < target 2.
	res, err := ReplaceBelowFloor(ctx, pool, cfg)
	require.NoError(t, err)
	require.Equal(t, 0, res.Demoted, "no demote until a trusted replacement exists")
	assertPinState(t, ctx, pool, "bf-cid", bfNodeA, "acked")

	// C acks a replacement → live holders excluding A = 2 (B, C) >= target 2.
	assignPinState(t, ctx, pool, "bf-cid", bfNodeC, "acked")
	res, err = ReplaceBelowFloor(ctx, pool, cfg)
	require.NoError(t, err)
	require.Equal(t, 1, res.Demoted, "A is demoted once the healthy count is restored")
	assertPinState(t, ctx, pool, "bf-cid", bfNodeA, "failed")
	// No durability dip: the two trusted replicas stay acked.
	assertPinState(t, ctx, pool, "bf-cid", bfNodeB, "acked")
	assertPinState(t, ctx, pool, "bf-cid", bfNodeC, "acked")
}

// TestBelowFloorSoleHolderNeverDemotes: a sustained node that is the ONLY holder
// is never demoted — it stays the repair source of last resort (contract item 4,
// the drain self-sourcing principle).
func TestBelowFloorSoleHolderNeverDemotes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedNode(t, ctx, pool, bfNodeA, "active", "current", true)
	seedBlob(t, ctx, pool, "sole-cid", "normal", true)
	assignPinState(t, ctx, pool, "sole-cid", bfNodeA, "acked")
	_, err := pool.Exec(ctx, `
		INSERT INTO blob_replication_state
			(cid, healthy_acked_count, sourceable_acked_count, in_flight_count,
			 target_count, safety_tier, local_recoverable, durability_class, dirty)
		VALUES ('sole-cid', 0, 0, 0, 2, 'donor_lost', true, 'normal', false)`)
	require.NoError(t, err)
	markBelowFloor(t, ctx, pool, bfNodeA, bfSustainedSecs)

	res, err := ReplaceBelowFloor(ctx, pool, bfSweepCfg(0.5, 0.1, bfGraceSecs, 500))
	require.NoError(t, err)
	require.Equal(t, 0, res.Demoted, "a sole below-floor holder is never demoted")
	assertPinState(t, ctx, pool, "sole-cid", bfNodeA, "acked")
}
