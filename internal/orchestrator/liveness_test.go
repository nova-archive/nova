package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/notify"
	"github.com/stretchr/testify/require"
)

// recordingNotifier captures emitted events for assertions.
type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recordingNotifier) Emit(_ context.Context, ev notify.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingNotifier) count(typ string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// ageNode backdates a node's last_seen_at so the sweeper's timers fire.
func ageNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`UPDATE nodes SET last_seen_at = now() - make_interval(secs => $2) WHERE id = $1::uuid`,
		id, age.Seconds())
	require.NoError(t, err)
}

func nodeStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var s string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM nodes WHERE id=$1::uuid`, id).Scan(&s))
	return s
}

// testTimers: suspect at 30s (3×10s), unreachable at 60s, evicted at 120s.
func testTimers() LivenessConfig {
	return LivenessConfig{
		HeartbeatInterval:  10 * time.Second,
		SuspectAfterMissed: 3,
		UnreachableAfter:   60 * time.Second,
		EvictedAfter:       120 * time.Second,
	}
}

func TestSweeperTimerTransitions(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	fresh := "11111111-0000-0000-0000-000000000001"
	susp := "11111111-0000-0000-0000-000000000002"
	unre := "11111111-0000-0000-0000-000000000003"
	evic := "11111111-0000-0000-0000-000000000004"
	for _, id := range []string{fresh, susp, unre, evic} {
		seedNode(t, ctx, pool, id, "active", "current", false)
	}
	ageNode(t, ctx, pool, fresh, 5*time.Second)  // within all timers → stays active
	ageNode(t, ctx, pool, susp, 40*time.Second)  // past suspect, before unreachable
	ageNode(t, ctx, pool, unre, 90*time.Second)  // past unreachable, before evicted
	ageNode(t, ctx, pool, evic, 200*time.Second) // past evicted

	res, err := ReconcileNodeLiveness(ctx, pool, testTimers(), notify.NoopNotifier{})
	require.NoError(t, err)
	require.Equal(t, 1, res.ToSuspect)
	require.Equal(t, 1, res.ToUnreachable)
	require.Equal(t, 1, res.ToEvicted)

	require.Equal(t, "active", nodeStatus(t, ctx, pool, fresh))
	require.Equal(t, "suspect", nodeStatus(t, ctx, pool, susp))
	require.Equal(t, "unreachable", nodeStatus(t, ctx, pool, unre))
	require.Equal(t, "evicted", nodeStatus(t, ctx, pool, evic))
}

func TestUnreachableEnqueuesAffectedCIDs(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	tg := ReplicationTargets{Important: 5, Normal: 3, Cache: 2}

	node := "22222222-0000-0000-0000-000000000001"
	seedNode(t, ctx, pool, node, "active", "current", false)
	seedBlob(t, ctx, pool, "ack-cid", "normal", false)
	seedBlob(t, ctx, pool, "pend-cid", "normal", false)
	assignPinState(t, ctx, pool, "ack-cid", node, "acked")
	assignPinState(t, ctx, pool, "pend-cid", node, "pending")
	// Create a projection row so we can observe dirty flip.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, RecomputeCID(ctx, tx, "ack-cid", tg))
	require.NoError(t, tx.Commit(ctx))

	ageNode(t, ctx, pool, node, 90*time.Second)
	_, err = ReconcileNodeLiveness(ctx, pool, testTimers(), notify.NoopNotifier{})
	require.NoError(t, err)

	require.Equal(t, "unreachable", nodeStatus(t, ctx, pool, node))

	// Both CIDs enqueued for recompute.
	var queued int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blob_replication_reconcile_queue WHERE cid IN ('ack-cid','pend-cid')`).Scan(&queued))
	require.Equal(t, 2, queued)

	// Projection row marked dirty.
	var dirty bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT dirty FROM blob_replication_state WHERE cid='ack-cid'`).Scan(&dirty))
	require.True(t, dirty, "unreachable transition marks affected projection rows dirty")

	// Pending reservation failed; acked row retained.
	var pendState, ackState string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT state FROM pin_assignments WHERE cid='pend-cid' AND node_id=$1::uuid`, node).Scan(&pendState))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT state FROM pin_assignments WHERE cid='ack-cid' AND node_id=$1::uuid`, node).Scan(&ackState))
	require.Equal(t, "failed", pendState, "dead in-flight reservation freed for re-scheduling")
	require.Equal(t, "acked", ackState, "acked rows are retained on unreachable")
}

func TestEvictedDeletesAssignmentsAfterEnqueue(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	node := "33333333-0000-0000-0000-000000000001"
	seedNode(t, ctx, pool, node, "active", "current", false)
	seedBlob(t, ctx, pool, "e-ack", "normal", false)
	seedBlob(t, ctx, pool, "e-pend", "normal", false)
	assignPinState(t, ctx, pool, "e-ack", node, "acked")
	assignPinState(t, ctx, pool, "e-pend", node, "pending")

	ageNode(t, ctx, pool, node, 200*time.Second)
	_, err := ReconcileNodeLiveness(ctx, pool, testTimers(), notify.NoopNotifier{})
	require.NoError(t, err)

	require.Equal(t, "evicted", nodeStatus(t, ctx, pool, node))

	// Affected CIDs enqueued BEFORE the rows were deleted.
	var queued int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blob_replication_reconcile_queue WHERE cid IN ('e-ack','e-pend')`).Scan(&queued))
	require.Equal(t, 2, queued, "evicted node's CIDs enqueued before delete")

	// All assignments deleted (D-M5-4a-EVICT: no retired state, so delete).
	var remaining int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pin_assignments WHERE node_id=$1::uuid`, node).Scan(&remaining))
	require.Equal(t, 0, remaining)
}

func TestRevokeRetainsRowsNonCountingEnqueues(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	node := "44444444-0000-0000-0000-000000000001"
	seedNode(t, ctx, pool, node, "active", "current", false)
	seedBlob(t, ctx, pool, "r-cid", "normal", false)
	assignPinState(t, ctx, pool, "r-cid", node, "acked")
	// novactl revoke is DB-direct.
	_, err := pool.Exec(ctx,
		`UPDATE nodes SET status='revoked', cert_revoked_at=now() WHERE id=$1::uuid`, node)
	require.NoError(t, err)

	rec := &recordingNotifier{}
	_, err = ReconcileNodeLiveness(ctx, pool, testTimers(), rec)
	require.NoError(t, err)

	require.Equal(t, 1, rec.count("federation.node_revoked"), "node_revoked emitted once")
	require.Equal(t, node, rec.events[0].ScopeKey, "scope key is the node id")

	// Row retained (forensic evidence), CID enqueued, signaled timestamp set.
	var rows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pin_assignments WHERE node_id=$1::uuid`, node).Scan(&rows))
	require.Equal(t, 1, rows, "revoked node's rows are retained")

	var queued int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blob_replication_reconcile_queue WHERE cid='r-cid'`).Scan(&queued))
	require.Equal(t, 1, queued)

	var signaled bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT revoked_signaled_at IS NOT NULL FROM nodes WHERE id=$1::uuid`, node).Scan(&signaled))
	require.True(t, signaled)
}

func TestRevokedSignaledExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	node := "55555555-0000-0000-0000-000000000001"
	seedNode(t, ctx, pool, node, "active", "current", false)
	_, err := pool.Exec(ctx,
		`UPDATE nodes SET status='revoked', cert_revoked_at=now() WHERE id=$1::uuid`, node)
	require.NoError(t, err)

	rec := &recordingNotifier{}
	_, err = ReconcileNodeLiveness(ctx, pool, testTimers(), rec)
	require.NoError(t, err)
	_, err = ReconcileNodeLiveness(ctx, pool, testTimers(), rec)
	require.NoError(t, err)

	require.Equal(t, 1, rec.count("federation.node_revoked"), "second sweep does not re-signal")
}
