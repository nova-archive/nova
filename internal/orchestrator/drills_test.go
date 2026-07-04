package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/notify"
	"github.com/stretchr/testify/require"
)

// P2-M7 operational drills (D-M7-4): each test is the automated proof half of
// a runbook row (docs/runbooks/). What each drill does NOT prove is noted
// inline — the transfer transport itself is the m5/m7 e2e capstones' job.

var drillLiveness = LivenessConfig{
	HeartbeatInterval:  time.Minute,
	SuspectAfterMissed: 3,
	UnreachableAfter:   time.Hour,
	EvictedAfter:       30 * 24 * time.Hour,
}

func freshenAllNodes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at = now()`)
	require.NoError(t, err)
}

// TestDrillRevocationHeals: revoke → cert-holder drops from durability
// instantly, CIDs reconcile with reason node_revoked, and the revocation event
// is signaled exactly once. NOT proven here: the repair transfer itself.
func TestDrillRevocationHeals(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	seedHealBlob(t, ctx, pool, "rv-x", "normal")
	a := "d0000000-0000-0000-0000-00000000000a"
	b := "d0000000-0000-0000-0000-00000000000b"
	c := "d0000000-0000-0000-0000-00000000000c"
	for _, id := range []string{a, b, c} {
		seedNode(t, ctx, pool, id, "active", "current", false)
	}
	configSource(t, ctx, pool, a, "rv-x", 1000000, 1.0)
	configSource(t, ctx, pool, b, "rv-x", 1000000, 1.0)
	configDest(t, ctx, pool, c)
	freshenAllNodes(t, ctx, pool)
	q := gen.New(pool)

	before, err := q.RecomputeReplicationCounts(ctx, "rv-x")
	require.NoError(t, err)
	require.EqualValues(t, 2, before.HealthyAcked)

	// Fault: the novactl revoke path is DB-direct.
	aPg := pgUUID(t, a)
	n, err := q.RevokeNode(ctx, aPg)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	// The sweeper is the coordinator-local observer of DB-direct revocation.
	res, err := ReconcileNodeLiveness(ctx, pool, drillLiveness, notify.NoopNotifier{})
	require.NoError(t, err)
	require.Equal(t, 1, res.Revoked, "revocation observed exactly once")

	var reason string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT reason FROM blob_replication_reconcile_queue WHERE cid='rv-x'`).Scan(&reason))
	require.Equal(t, "node_revoked", reason)
	var signaled bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT revoked_signaled_at IS NOT NULL FROM nodes WHERE id=$1::uuid`, a).Scan(&signaled))
	require.True(t, signaled, "emit-once marker set")

	after, err := q.RecomputeReplicationCounts(ctx, "rv-x")
	require.NoError(t, err)
	require.EqualValues(t, 1, after.HealthyAcked, "revoked A dropped from durability instantly")

	// Re-sweep: no double-signal.
	res2, err := ReconcileNodeLiveness(ctx, pool, drillLiveness, notify.NoopNotifier{})
	require.NoError(t, err)
	require.Equal(t, 0, res2.Revoked)
}

// TestDrillProviderLossHeals: an entire verified failure domain goes silent →
// both nodes sweep to unreachable, the CID surfaces as donor_lost, and the
// scheduler's next pick places the replacement into the SURVIVING domain via
// the coordinator-as-source emergency path (the blob is local-recoverable).
func TestDrillProviderLossHeals(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	targets := ReplicationTargets{Important: 5, Normal: 2, Cache: 2}

	// Blob with a LOCAL copy (emergency source when every donor holder is gone).
	seedBlob(t, ctx, pool, "pl-x", "normal", true)
	_, err := pool.Exec(ctx,
		`INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		 VALUES ('pl-x','sha2-256','raw','size-262144',100,100,1)`)
	require.NoError(t, err)

	a1 := "e0000000-0000-0000-0000-0000000000a1"
	a2 := "e0000000-0000-0000-0000-0000000000a2"
	b1 := "e0000000-0000-0000-0000-0000000000b1"
	b2 := "e0000000-0000-0000-0000-0000000000b2"
	for _, id := range []string{a1, a2, b1, b2} {
		seedNode(t, ctx, pool, id, "active", "current", false)
	}
	configSource(t, ctx, pool, a1, "pl-x", 1000000, 1.0)
	configSource(t, ctx, pool, a2, "pl-x", 1000000, 1.0)
	configDest(t, ctx, pool, b1)
	configDest(t, ctx, pool, b2)
	// Operator-verified failure domains: P1 = {a1,a2}, P2 = {b1,b2}.
	for id, dom := range map[string]string{a1: "provider-1", a2: "provider-1", b1: "provider-2", b2: "provider-2"} {
		_, err := pool.Exec(ctx,
			`UPDATE nodes SET failure_domain_id=$2, operator_verified_at=now() WHERE id=$1::uuid`, id, dom)
		require.NoError(t, err)
	}
	freshenAllNodes(t, ctx, pool)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, RecomputeCID(ctx, tx, "pl-x", targets))
	require.NoError(t, tx.Commit(ctx))
	require.Equal(t, "healthy", tierOf(t, ctx, pool, "pl-x"))

	// Fault: provider-1 goes dark past the unreachable threshold.
	ageNode(t, ctx, pool, a1, 2*time.Hour)
	ageNode(t, ctx, pool, a2, 2*time.Hour)
	res, err := ReconcileNodeLiveness(ctx, pool, drillLiveness, notify.NoopNotifier{})
	require.NoError(t, err)
	require.Equal(t, 2, res.ToUnreachable, "both provider-1 nodes swept unreachable")
	var reason string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT reason FROM blob_replication_reconcile_queue WHERE cid='pl-x'`).Scan(&reason))
	require.Equal(t, "node_unreachable", reason)

	// Heal: the tick drains the queue (recompute → donor_lost is visible) and
	// places the replacement into the surviving domain.
	sch := NewScheduler(pool, SchedulerConfig{Targets: targets, ReputationFloor: 0.5})
	n, err := sch.Tick(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1, "a replacement reservation was made")

	dest, ok := pendingDestFor(t, ctx, pool, "pl-x")
	require.True(t, ok, "pending replacement exists")
	require.Contains(t, []string{b1, b2}, dest, "placement lands in the surviving domain")
	var srcCol *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT source_node_id::text FROM pin_assignments WHERE cid='pl-x' AND state='pending'`).Scan(&srcCol))
	require.Nil(t, srcCol, "coordinator-as-source emergency path (no donor holder survives)")
}

// TestDrillDrainedSoleHolderHealsFromDrainingSource: the DB half of the drain
// e2e capstone — draining the SOLE holder drops the CID to donor_lost, and the
// tick still heals it, sourcing FROM the draining node (repair source of last
// resort, D-M7-6c). Without donor_lost in the tick's walk this CID would
// strand and drain debt could never clear.
func TestDrillDrainedSoleHolderHealsFromDrainingSource(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	// target 1: drain of the sole holder forces the 1→2-copy repair, and the
	// debt clears once ONE non-draining holder acks (D-M7-6f arithmetic).
	targets := ReplicationTargets{Important: 5, Normal: 1, Cache: 1}

	seedHealBlob(t, ctx, pool, "dr-x", "normal")
	a := "f0000000-0000-0000-0000-00000000000a"
	b := "f0000000-0000-0000-0000-00000000000b"
	seedNode(t, ctx, pool, a, "active", "current", false)
	seedNode(t, ctx, pool, b, "active", "current", false)
	configSource(t, ctx, pool, a, "dr-x", 1000000, 1.0)
	configDest(t, ctx, pool, b)
	freshenAllNodes(t, ctx, pool)
	q := gen.New(pool)
	aPg := pgUUID(t, a)

	// Projection row first (target_count feeds the drain-debt query).
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, RecomputeCID(ctx, tx, "dr-x", targets))
	require.NoError(t, tx.Commit(ctx))

	// Drain A: the same statement sequence the CLI core runs.
	markDraining(t, ctx, pool, a)
	_, err = q.FailNodePendingAssignments(ctx, aPg)
	require.NoError(t, err)
	require.NoError(t, q.MarkReplicationDirtyForNode(ctx, aPg))
	require.NoError(t, q.EnqueueReconcileForNode(ctx, gen.EnqueueReconcileForNodeParams{
		Reason: "node_draining", NodeID: aPg,
	}))

	debt, err := q.CountDrainPendingCIDs(ctx, aPg)
	require.NoError(t, err)
	require.EqualValues(t, 1, debt, "sole-holder drain opens debt")

	sch := NewScheduler(pool, SchedulerConfig{Targets: targets, ReputationFloor: 0.5})
	n, err := sch.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the donor_lost CID is healed, not stranded")

	var state, srcCol string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT state, source_node_id::text FROM pin_assignments WHERE cid='dr-x' AND node_id=$1::uuid`, b).
		Scan(&state, &srcCol))
	require.Equal(t, "pending", state)
	require.Equal(t, a, srcCol, "repair is sourced FROM the draining node (last resort)")

	// B acks → drain debt clears (safe-to-revoke gate input).
	var bAssign uuid.UUID
	var bGen int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT assignment_id, generation FROM pin_assignments WHERE cid='dr-x' AND node_id=$1::uuid`, b).
		Scan(&bAssign, &bGen))
	acked, err := q.AckPinAssignment(ctx, gen.AckPinAssignmentParams{
		Cid: "dr-x", NodeID: pgUUID(t, b),
		AssignmentID: pgUUID(t, bAssign.String()), Generation: bGen,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, acked)

	debt, err = q.CountDrainPendingCIDs(ctx, aPg)
	require.NoError(t, err)
	require.EqualValues(t, 0, debt, "drain-ready once the replacement acks")
}
