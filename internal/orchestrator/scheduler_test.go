package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/node/bandwidth"
	"github.com/stretchr/testify/require"
)

var healTargets = ReplicationTargets{Important: 5, Normal: 3, Cache: 2}

// seedHealBlob seeds a blob + its manifest (GetBlobSize/AssignPinWithSource read
// blob_manifests) + its blob_storage_state row (RecomputeCID reads durability_class).
func seedHealBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid, class string) {
	t.Helper()
	seedBlob(t, ctx, pool, cid, class, false)
	_, err := pool.Exec(ctx,
		`INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		 VALUES ($1,'sha2-256','raw','size-262144',100,100,1)`, cid)
	require.NoError(t, err)
}

// configSource makes node id a repair-sourceable acked holder of cid.
func configSource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, cid string, remaining int64, reputation float32) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		UPDATE nodes SET status='active', assignment_sync_state='current', trust_state='trusted',
			advertised_capabilities='{read-source/v1,repair-stream/v1}'::text[],
			source_nebula_addr='10.0.0.1:9443', last_egress_remaining_bytes=$2,
			last_free_bytes=1000000000, reputation_score=$3
		WHERE id=$1::uuid`, id, remaining, reputation)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO pin_assignments (cid,node_id,state) VALUES ($1,$2::uuid,'acked')`, cid, id)
	require.NoError(t, err)
}

// configDest makes node id an eligible (non-holder) placement destination.
func configDest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		UPDATE nodes SET status='active', assignment_sync_state='current', trust_state='trusted',
			last_free_bytes=1000000000, reputation_score=1.0, placement_weight=1.0
		WHERE id=$1::uuid`, id)
	require.NoError(t, err)
}

func recompute(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, RecomputeCID(ctx, tx, cid, healTargets))
	require.NoError(t, tx.Commit(ctx))
}

func tierOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string) string {
	t.Helper()
	var tier string
	require.NoError(t, pool.QueryRow(ctx, `SELECT safety_tier FROM blob_replication_state WHERE cid=$1`, cid).Scan(&tier))
	return tier
}

func pendingDestFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string) (string, bool) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT node_id::text FROM pin_assignments WHERE cid=$1 AND state='pending'`, cid)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		return id, true
	}
	return "", false
}

func TestStepCapacityHintDonorStillRefuses(t *testing.T) {
	// step_capacity is a best-effort HINT (D-M5-6-TEL): a donor whose reported
	// remaining looks generous can still refuse at its authoritative bucket. Here
	// the hint would say "1000 available" but the bucket refuses an over-budget
	// repair — proving the coordinator reserves optimistically, the donor decides.
	now := time.Unix(3_000_000, 0)
	b := bandwidth.NewDailyBucket(1000, now)
	require.Equal(t, int64(1000), b.Remaining(now), "hint reports 1000 available")
	require.False(t, b.Take(5000, now), "the bucket still refuses an over-budget repair")
}

func TestImportantBelowFiveWarns(t *testing.T) {
	require.NotEmpty(t, WarnIfImportantBelowFive(3), "important<5 warns")
	require.NotEmpty(t, WarnIfImportantBelowFive(4))
	require.Empty(t, WarnIfImportantBelowFive(5), "5 is the recommendation")
	require.Empty(t, WarnIfImportantBelowFive(7))
}

func TestSourceSelectionMaxCapacityReputationRepairSourceableOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedHealBlob(t, ctx, pool, "s1", "normal")

	a := "a0000000-0000-0000-0000-000000000001"
	b := "b0000000-0000-0000-0000-000000000002"
	c := "c0000000-0000-0000-0000-000000000003"
	seedNode(t, ctx, pool, a, "active", "current", false)
	seedNode(t, ctx, pool, b, "active", "current", false)
	seedNode(t, ctx, pool, c, "active", "current", false)
	configSource(t, ctx, pool, a, "s1", 1000, 1.0) // weight 1000
	configSource(t, ctx, pool, b, "s1", 5000, 1.0) // weight 5000 — should win
	configSource(t, ctx, pool, c, "s1", 9999, 1.0) // highest remaining...
	// ...but c advertises read-source ONLY, so it is not repair-sourceable.
	_, err := pool.Exec(ctx, `UPDATE nodes SET advertised_capabilities='{read-source/v1}'::text[] WHERE id=$1::uuid`, c)
	require.NoError(t, err)

	row, err := gen.New(pool).ListRepairSourceHolders(ctx, gen.ListRepairSourceHoldersParams{
		Cid: "s1", Size: pgtype.Int8{Int64: 100, Valid: true},
	})
	require.NoError(t, err)
	require.Equal(t, b, uuid.UUID(row.NodeID.Bytes).String(), "max remaining×reputation among repair-sourceable holders")
}

func TestSchedulerHealsTier1ReservesPending(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedHealBlob(t, ctx, pool, "h1", "normal")
	src := "a1000000-0000-0000-0000-000000000001"
	dest := "b1000000-0000-0000-0000-000000000002"
	seedNode(t, ctx, pool, src, "active", "current", false)
	seedNode(t, ctx, pool, dest, "active", "current", false)
	configSource(t, ctx, pool, src, "h1", 1000000, 1.0)
	configDest(t, ctx, pool, dest)
	recompute(t, ctx, pool, "h1")
	require.Equal(t, "tier1", tierOf(t, ctx, pool, "h1"))

	sch := NewScheduler(pool, SchedulerConfig{Targets: healTargets, ReputationFloor: 0.5})
	n, err := sch.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "one reservation made")

	var state, srcCol string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT state, source_node_id::text FROM pin_assignments WHERE cid='h1' AND node_id=$1::uuid`, dest).
		Scan(&state, &srcCol))
	require.Equal(t, "pending", state)
	require.Equal(t, src, srcCol, "reservation bound to the selected repair source")
}

func TestStrictTier1BeforeTier2(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	// Tier-1 CID: one acked holder. Tier-2 CID: two acked holders.
	seedHealBlob(t, ctx, pool, "t1", "normal")
	seedHealBlob(t, ctx, pool, "t2", "normal")
	s1 := "a2000000-0000-0000-0000-000000000001"
	s2a := "a2000000-0000-0000-0000-000000000002"
	s2b := "a2000000-0000-0000-0000-000000000003"
	dest := "b2000000-0000-0000-0000-000000000009"
	for _, id := range []string{s1, s2a, s2b, dest} {
		seedNode(t, ctx, pool, id, "active", "current", false)
	}
	configSource(t, ctx, pool, s1, "t1", 1000000, 1.0)
	configSource(t, ctx, pool, s2a, "t2", 1000000, 1.0)
	configSource(t, ctx, pool, s2b, "t2", 1000000, 1.0)
	configDest(t, ctx, pool, dest)
	recompute(t, ctx, pool, "t1")
	recompute(t, ctx, pool, "t2")
	require.Equal(t, "tier1", tierOf(t, ctx, pool, "t1"))
	require.Equal(t, "tier2", tierOf(t, ctx, pool, "t2"))

	sch := NewScheduler(pool, SchedulerConfig{Targets: healTargets, ReputationFloor: 0.5})
	_, err := sch.Tick(ctx)
	require.NoError(t, err)

	_, t1Healed := pendingDestFor(t, ctx, pool, "t1")
	_, t2Healed := pendingDestFor(t, ctx, pool, "t2")
	require.True(t, t1Healed, "Tier-1 healed")
	require.False(t, t2Healed, "Tier-2 untouched while Tier-1 work existed this tick")
}

func TestPendingDoesNotLiftTier1(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedHealBlob(t, ctx, pool, "p1", "normal")
	holder := "a3000000-0000-0000-0000-000000000001"
	pend := "a3000000-0000-0000-0000-000000000002"
	seedNode(t, ctx, pool, holder, "active", "current", false)
	seedNode(t, ctx, pool, pend, "active", "current", false)
	configSource(t, ctx, pool, holder, "p1", 1000000, 1.0) // 1 acked ⇒ healthy=1
	// A live pending reservation must NOT count toward durability.
	_, err := pool.Exec(ctx, `INSERT INTO pin_assignments (cid,node_id,state) VALUES ('p1',$1::uuid,'pending')`, pend)
	require.NoError(t, err)
	recompute(t, ctx, pool, "p1")

	require.Equal(t, "tier1", tierOf(t, ctx, pool, "p1"), "pending does not lift Tier-1")
}

func TestSchedulerRecomputesDirtyBeforeReserving(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedHealBlob(t, ctx, pool, "d1", "normal")
	// Authority: 3 acked holders ⇒ actually healthy at target 3.
	for i, id := range []string{
		"a4000000-0000-0000-0000-000000000001",
		"a4000000-0000-0000-0000-000000000002",
		"a4000000-0000-0000-0000-000000000003",
	} {
		seedNode(t, ctx, pool, id, "active", "current", false)
		configSource(t, ctx, pool, id, "d1", 1000000, 1.0)
		_ = i
	}
	dest := "b4000000-0000-0000-0000-000000000009"
	seedNode(t, ctx, pool, dest, "active", "current", false)
	configDest(t, ctx, pool, dest)

	// Insert a STALE, dirty projection row claiming Tier-1 (healthy=1) — the kind a
	// bulk transition leaves behind before the drain catches up.
	_, err := pool.Exec(ctx, `
		INSERT INTO blob_replication_state
			(cid, healthy_acked_count, sourceable_acked_count, in_flight_count, target_count,
			 safety_tier, local_recoverable, durability_class, dirty)
		VALUES ('d1', 1, 1, 0, 3, 'tier1', false, 'normal', true)`)
	require.NoError(t, err)

	sch := NewScheduler(pool, SchedulerConfig{Targets: healTargets, ReputationFloor: 0.5})
	n, err := sch.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "recompute reveals the CID is actually healthy ⇒ no reservation")
	_, healed := pendingDestFor(t, ctx, pool, "d1")
	require.False(t, healed, "must not double-assign from a stale dirty row")
}

func TestRestartRederivesTiersFromProjection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedHealBlob(t, ctx, pool, "r1", "normal")
	src := "a5000000-0000-0000-0000-000000000001"
	dest := "b5000000-0000-0000-0000-000000000002"
	seedNode(t, ctx, pool, src, "active", "current", false)
	seedNode(t, ctx, pool, dest, "active", "current", false)
	configSource(t, ctx, pool, src, "r1", 1000000, 1.0)
	configDest(t, ctx, pool, dest)
	recompute(t, ctx, pool, "r1")

	// A brand-new Scheduler (no in-memory state) heals purely from the projection.
	fresh := NewScheduler(pool, SchedulerConfig{Targets: healTargets, ReputationFloor: 0.5})
	n, err := fresh.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "restart re-derives the Tier-1 set from the projection")
}
