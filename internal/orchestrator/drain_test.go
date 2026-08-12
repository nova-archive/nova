package orchestrator

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// P2-M7 drain query classification (D-M7-6b/6c): safety counts EXCLUDE a
// draining node; placement never targets it; read/repair source SELECTION may
// still use it, deprioritized. One fixture throughout: CID "drain-cid" with
// target_count=2, acked on nodes A (to be drained) and B (healthy); node C is
// an eligible empty destination.

const (
	drainNodeA = "aaaaaaaa-aaaa-aaaa-aaaa-000000000001"
	drainNodeB = "aaaaaaaa-aaaa-aaaa-aaaa-000000000002"
	drainNodeC = "aaaaaaaa-aaaa-aaaa-aaaa-000000000003"
)

func pgUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	require.NoError(t, id.Scan(s))
	return id
}

// seedDrainFixture: blob drain-cid (target 2), A+B acked holders, C empty
// eligible destination; all three live/current/read-sourceable and fresh.
func seedDrainFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	seedBlob(t, ctx, pool, "drain-cid", "normal", true)
	for _, n := range []string{drainNodeA, drainNodeB, drainNodeC} {
		seedNode(t, ctx, pool, n, "active", "current", true)
	}
	_, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at = now()`)
	require.NoError(t, err)
	assignPinState(t, ctx, pool, "drain-cid", drainNodeA, "acked")
	assignPinState(t, ctx, pool, "drain-cid", drainNodeB, "acked")
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_replication_state
			(cid, healthy_acked_count, sourceable_acked_count, in_flight_count,
			 target_count, safety_tier, local_recoverable, durability_class, dirty)
		VALUES ('drain-cid', 2, 2, 0, 2, 'healthy', true, 'normal', false)`)
	require.NoError(t, err)
}

func markDraining(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) {
	t.Helper()
	n, err := gen.New(pool).SetNodeDraining(ctx, pgUUID(t, id))
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "SetNodeDraining must mark a non-draining node")
}

func TestDrainExcludedFromSafetyCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedDrainFixture(t, ctx, pool)
	q := gen.New(pool)

	before, err := q.RecomputeReplicationCounts(ctx, gen.RecomputeReplicationCountsParams{
		Cid: "drain-cid", BelowFloorGraceSecs: DefaultBelowFloorGraceSeconds,
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, before.HealthyAcked)
	require.EqualValues(t, 2, before.SourceableAcked)

	markDraining(t, ctx, pool, drainNodeA)

	after, err := q.RecomputeReplicationCounts(ctx, gen.RecomputeReplicationCountsParams{
		Cid: "drain-cid", BelowFloorGraceSecs: DefaultBelowFloorGraceSeconds,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, after.HealthyAcked, "draining A must not be durability-countable (D-M7-6b)")
	require.EqualValues(t, 1, after.SourceableAcked, "draining A must not be safety-sourceable (D-M7-6b)")

	// The commit/prune/read safety count (storage_state.sql) must agree.
	n, err := q.CountSourceableHolders(ctx, gen.CountSourceableHoldersParams{
		Cid: "drain-cid", StaleSecs: 3600,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "CountSourceableHolders must exclude draining A")
}

func TestDrainExcludedFromPlacement(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedDrainFixture(t, ctx, pool)
	markDraining(t, ctx, pool, drainNodeA)

	// A CID nobody holds: A/B/C are all non-holders, but A is draining.
	rows, err := gen.New(pool).ListPlacementCandidates(ctx, gen.ListPlacementCandidatesParams{
		Cid: "other-cid", BelowFloorGraceSecs: DefaultBelowFloorGraceSeconds,
	})
	require.NoError(t, err)
	got := map[string]bool{}
	for _, r := range rows {
		got[uuidString(r.NodeID)] = true
	}
	require.False(t, got[drainNodeA], "draining node must never be a placement destination (D-M7-6c)")
	require.True(t, got[drainNodeC], "eligible empty node stays a destination")
	require.True(t, got[drainNodeB])
}

func TestDrainDeprioritizedNotExcludedFromSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedDrainFixture(t, ctx, pool)
	q := gen.New(pool)

	// A outranks B on reputation — the prepended drain sort key must still win.
	_, err := pool.Exec(ctx, `UPDATE nodes SET reputation_score = 0.9 WHERE id = $1::uuid`, drainNodeA)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE nodes SET reputation_score = 0.5 WHERE id = $1::uuid`, drainNodeB)
	require.NoError(t, err)
	markDraining(t, ctx, pool, drainNodeA)

	holders, err := q.ListSourceableHolders(ctx, gen.ListSourceableHoldersParams{
		Cid: "drain-cid", StaleSecs: 3600,
	})
	require.NoError(t, err)
	require.Len(t, holders, 2, "draining A stays selectable as a read source")
	require.Equal(t, drainNodeB, uuidString(holders[0].NodeID), "healthy B sorts first despite lower reputation")
	require.Equal(t, drainNodeA, uuidString(holders[1].NodeID), "draining A sorts last")

	// Repair source of last resort: a CID whose ONLY acked holder is draining A.
	seedBlob(t, ctx, pool, "repair-cid", "normal", true)
	assignPinState(t, ctx, pool, "repair-cid", drainNodeA, "acked")
	_, err = pool.Exec(ctx, `
		UPDATE nodes SET advertised_capabilities = '{read-source/v1,repair-stream/v1,blob-transfer/v1}', effective_capabilities = '{read-source/v1,repair-stream/v1,blob-transfer/v1}'
		WHERE id = $1::uuid`, drainNodeA)
	require.NoError(t, err)
	src, err := q.ListRepairSourceHolders(ctx, gen.ListRepairSourceHoldersParams{
		Cid: "repair-cid", Size: pgtype.Int8{Int64: 100, Valid: true},
	})
	require.NoError(t, err)
	require.Equal(t, drainNodeA, uuidString(src.NodeID),
		"a draining node remains a repair source of last resort (D-M7-6c)")
}

func TestWeightZeroIsNotDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedDrainFixture(t, ctx, pool)
	q := gen.New(pool)

	// B: weight-zero throttle, NOT draining. A: full weight, draining.
	// The two markers must stay distinct in BOTH directions (Global Constraint):
	// healthy == 1 proves B still counts AND A does not.
	_, err := pool.Exec(ctx, `UPDATE nodes SET placement_weight = 0.0 WHERE id = $1::uuid`, drainNodeB)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE nodes SET placement_weight = 1.0 WHERE id = $1::uuid`, drainNodeA)
	require.NoError(t, err)
	markDraining(t, ctx, pool, drainNodeA)

	counts, err := q.RecomputeReplicationCounts(ctx, gen.RecomputeReplicationCountsParams{
		Cid: "drain-cid", BelowFloorGraceSecs: DefaultBelowFloorGraceSeconds,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, counts.HealthyAcked,
		"weight-zero B still counts; draining A does not")
}

func TestDrainDebtCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedDrainFixture(t, ctx, pool)
	q := gen.New(pool)
	aID := pgUUID(t, drainNodeA)

	markDraining(t, ctx, pool, drainNodeA)

	// Only B (1 non-draining acked holder) < target 2 → debt 1.
	debt, err := q.CountDrainPendingCIDs(ctx, gen.CountDrainPendingCIDsParams{NodeID: aID, BelowFloorGraceSecs: config.DefaultBelowFloorGraceSeconds})
	require.NoError(t, err)
	require.EqualValues(t, 1, debt)

	// C acks a replacement → debt cleared.
	assignPinState(t, ctx, pool, "drain-cid", drainNodeC, "acked")
	debt, err = q.CountDrainPendingCIDs(ctx, gen.CountDrainPendingCIDsParams{NodeID: aID, BelowFloorGraceSecs: config.DefaultBelowFloorGraceSeconds})
	require.NoError(t, err)
	require.EqualValues(t, 0, debt)

	// Reset: C only PENDING — pending is never safe, so debt stays 1, but the
	// in-flight counter shows the replacement is being worked (D-M7-6f).
	_, err = pool.Exec(ctx, `DELETE FROM pin_assignments WHERE cid='drain-cid' AND node_id=$1::uuid`, drainNodeC)
	require.NoError(t, err)
	assignPinState(t, ctx, pool, "drain-cid", drainNodeC, "pending")
	debt, err = q.CountDrainPendingCIDs(ctx, gen.CountDrainPendingCIDsParams{NodeID: aID, BelowFloorGraceSecs: config.DefaultBelowFloorGraceSeconds})
	require.NoError(t, err)
	require.EqualValues(t, 1, debt, "pending never reduces drain debt")
	inflight, err := q.CountDrainInflightCIDs(ctx, gen.CountDrainInflightCIDsParams{NodeID: aID, BelowFloorGraceSecs: config.DefaultBelowFloorGraceSeconds})
	require.NoError(t, err)
	require.EqualValues(t, 1, inflight)

	// A pending on an INELIGIBLE destination (draining C) is not progress.
	_, err = pool.Exec(ctx, `UPDATE nodes SET draining_at = now() WHERE id = $1::uuid`, drainNodeC)
	require.NoError(t, err)
	inflight, err = q.CountDrainInflightCIDs(ctx, gen.CountDrainInflightCIDsParams{NodeID: aID, BelowFloorGraceSecs: config.DefaultBelowFloorGraceSeconds})
	require.NoError(t, err)
	require.EqualValues(t, 0, inflight, "pending on an ineligible destination must not read as drain progress")
}

func TestBelowFloorDebt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedDrainFixture(t, ctx, pool)
	q := gen.New(pool)

	// B sinks below the floor while holding 3 acked pins.
	_, err := pool.Exec(ctx, `UPDATE nodes SET reputation_score = 0.2 WHERE id = $1::uuid`, drainNodeB)
	require.NoError(t, err)
	seedBlob(t, ctx, pool, "bf-2", "normal", true)
	seedBlob(t, ctx, pool, "bf-3", "normal", true)
	assignPinState(t, ctx, pool, "bf-2", drainNodeB, "acked")
	assignPinState(t, ctx, pool, "bf-3", drainNodeB, "acked")

	rows, err := q.CountBelowFloorReplicas(ctx, 0.5)
	require.NoError(t, err)
	require.Len(t, rows, 1, "only B is below the floor")
	require.Equal(t, drainNodeB, uuidString(rows[0].NodeID))
	require.EqualValues(t, 3, rows[0].AckedReplicas)
}

func uuidString(id pgtype.UUID) string {
	v, err := id.Value()
	if err != nil {
		return ""
	}
	s, _ := v.(string)
	return s
}
