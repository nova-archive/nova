package orchestrator

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestSafetyTierBoundaries(t *testing.T) {
	// 0 healthy ⇒ donor_lost; 1 ⇒ tier1; 2..target-1 ⇒ tier2; ≥target ⇒ healthy.
	cases := []struct {
		healthy, target int
		want            string
	}{
		{0, 5, "donor_lost"},
		{1, 5, "tier1"},
		{2, 5, "tier2"},
		{4, 5, "tier2"},
		{5, 5, "healthy"},
		{6, 5, "healthy"},
		{0, 3, "donor_lost"},
		{1, 3, "tier1"},
		{2, 3, "tier2"},
		{3, 3, "healthy"},
		{0, 2, "donor_lost"},
		{1, 2, "tier1"},
		{2, 2, "healthy"},
		{1, 1, "healthy"}, // target=1: one copy is "at target", not tier1
	}
	for _, c := range cases {
		require.Equalf(t, c.want, safetyTier(c.healthy, c.target),
			"safetyTier(%d,%d)", c.healthy, c.target)
	}
}

func TestReplicationTargetsFor(t *testing.T) {
	tg := ReplicationTargets{Important: 5, Normal: 3, Cache: 2}
	require.Equal(t, 5, tg.For("important"))
	require.Equal(t, 3, tg.For("normal"))
	require.Equal(t, 2, tg.For("cache"))
}

// seedBlob inserts a blob + its blob_storage_state row (the durability_class /
// local-copy source of truth the recompute reads).
func seedBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid, class string, localPresent bool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ($1, 'image/jpeg', 1000, 'active', 'image', 2)`, cid)
	require.NoError(t, err)
	role := "absent"
	if localPresent {
		role = "origin"
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_storage_state (cid, commit_state, durability_class, local_role, local_present, local_bytes)
		VALUES ($1, 'committed', $2, $3, $4, 1100)`, cid, class, role, localPresent)
	require.NoError(t, err)
}

// seedNode inserts a node with the given liveness + sourceability attributes.
func seedNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, status, syncState string, readSourceable bool) {
	t.Helper()
	caps := "{}"
	addr := ""
	if readSourceable {
		caps = "{read-source/v1}"
		addr = "10.0.0.9:9443"
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint, capacity_bytes,
		                   bandwidth_budget_bytes_per_day, policy_filters, status,
		                   advertised_capabilities, assignment_sync_state, source_nebula_addr, trust_state)
		VALUES ($1::uuid, $2, $3, 1073741824, 1073741824, '{}', $4, $5::text[], $6, $7, 'trusted')`,
		id, id+"-nfp", id+"-ffp", status, caps, syncState, addr)
	require.NoError(t, err)
}

func assignPinState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid, nodeID, state string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO pin_assignments (cid, node_id, state) VALUES ($1, $2, $3)`, cid, nodeID, state)
	require.NoError(t, err)
}

func TestRecomputeCID_CountsCountableAckedOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	seedBlob(t, ctx, pool, "c1", "important", true)
	seedNode(t, ctx, pool, "11111111-1111-1111-1111-111111111111", "active", "current", true)       // counts + sourceable
	seedNode(t, ctx, pool, "22222222-2222-2222-2222-222222222222", "suspect", "current", false)     // counts, not sourceable
	seedNode(t, ctx, pool, "33333333-3333-3333-3333-333333333333", "unreachable", "current", true)  // excluded (status)
	seedNode(t, ctx, pool, "44444444-4444-4444-4444-444444444444", "active", "reconciling", true)   // excluded (sync)
	for _, n := range []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333", "44444444-4444-4444-4444-444444444444"} {
		assignPinState(t, ctx, pool, "c1", n, "acked")
	}

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, RecomputeCID(ctx, tx, "c1", ReplicationTargets{Important: 5, Normal: 3, Cache: 2}))
	require.NoError(t, tx.Commit(ctx))

	var healthy, sourceable, target int
	var tier, class string
	var localRec, dirty bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT healthy_acked_count, sourceable_acked_count, target_count, safety_tier,
		       durability_class, local_recoverable, dirty
		FROM blob_replication_state WHERE cid='c1'`).
		Scan(&healthy, &sourceable, &target, &tier, &class, &localRec, &dirty))

	require.Equal(t, 2, healthy, "only active+suspect+current acks count")
	require.Equal(t, 1, sourceable, "only the read-source-capable active node")
	require.Equal(t, 5, target)
	require.Equal(t, "tier2", tier)
	require.Equal(t, "important", class)
	require.True(t, localRec)
	require.False(t, dirty, "recompute clears dirty")
}

func TestInFlightExcludesDeadDestinations(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	seedBlob(t, ctx, pool, "c2", "normal", false)
	seedNode(t, ctx, pool, "aaaaaaaa-0000-0000-0000-000000000001", "active", "current", false)
	seedNode(t, ctx, pool, "bbbbbbbb-0000-0000-0000-000000000002", "unreachable", "current", false)
	assignPinState(t, ctx, pool, "c2", "aaaaaaaa-0000-0000-0000-000000000001", "pending")
	assignPinState(t, ctx, pool, "c2", "bbbbbbbb-0000-0000-0000-000000000002", "pending")

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, RecomputeCID(ctx, tx, "c2", ReplicationTargets{Important: 5, Normal: 3, Cache: 2}))
	require.NoError(t, tx.Commit(ctx))

	var inFlight int
	var tier string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT in_flight_count, safety_tier FROM blob_replication_state WHERE cid='c2'`).Scan(&inFlight, &tier))
	require.Equal(t, 1, inFlight, "pending on the unreachable node must not count")
	require.Equal(t, "donor_lost", tier, "0 healthy acks ⇒ donor_lost regardless of pending")
}

func TestDrainReconcileBoundedIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	tg := ReplicationTargets{Important: 5, Normal: 3, Cache: 2}

	for _, cid := range []string{"d1", "d2", "d3"} {
		seedBlob(t, ctx, pool, cid, "normal", false)
		_, err := pool.Exec(ctx, `INSERT INTO blob_replication_reconcile_queue (cid, reason) VALUES ($1,'test')`, cid)
		require.NoError(t, err)
	}

	done, err := DrainReconcile(ctx, pool, 2, tg)
	require.NoError(t, err)
	require.Equal(t, 2, done, "bounded to batch size")

	done, err = DrainReconcile(ctx, pool, 2, tg)
	require.NoError(t, err)
	require.Equal(t, 1, done, "remaining one")

	done, err = DrainReconcile(ctx, pool, 2, tg)
	require.NoError(t, err)
	require.Equal(t, 0, done, "idempotent: queue drained")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM blob_replication_state`).Scan(&n))
	require.Equal(t, 3, n, "all three CIDs got a projection row")
}
