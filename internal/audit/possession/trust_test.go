package possession

import (
	"context"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/notify"
	"github.com/stretchr/testify/require"
)

// TestRecordBelowFloorDoesNotBulkReplace verifies the D-M6-7 narrowing: a soft
// fail that drops a trusted node below the reputation floor demotes it to
// probationary, but it must NOT bulk-enqueue reconcile for the node's other
// still-acked CIDs (below-floor bulk re-replication is deferred to P2-M7). A soft
// (deadline) fail is not pin-specific, so NO reconcile rows are enqueued at all.
func TestRecordBelowFloorDoesNotBulkReplace(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	a := NewAuditor(pool, notify.NoopNotifier{}, testTrustConfig())

	node := seedNode(t, ctx, pool, 0.52, "trusted")

	// The audited CID.
	cid := seedBlob(t, ctx, pool)
	aid, generation := seedAckedPin(t, ctx, pool, cid, node)
	auditID := seedChallenge(t, ctx, pool, cid, node)

	// A second, unrelated acked pin on the same node that must remain untouched.
	otherCID := seedBlob(t, ctx, pool)
	_, _ = seedAckedPin(t, ctx, pool, otherCID, node)

	// 0.52 * 0.95 = 0.494, which crosses the 0.5 floor.
	res := DispatchResult{Outcome: OutcomeFailDeadline}
	require.NoError(t, a.Record(ctx, auditTarget(auditID, node, cid, aid, generation), res, 0.5))

	require.InDelta(t, 0.494, repScore(t, ctx, pool, node), 0.0005)
	require.Equal(t, "probationary", trustState(t, ctx, pool, node), "below-floor trusted node demotes to probationary")

	// The other pin is untouched and unreconciled (narrowing).
	require.Equal(t, "acked", pinState(t, ctx, pool, otherCID, node), "other acked pin must stay acked")
	require.Equal(t, 0, reconcileCount(t, ctx, pool, otherCID), "other CID must not be reconciled")
	// The audited (soft-failed) CID is likewise not reconciled, and nothing else is.
	require.Equal(t, 0, reconcileTotal(t, ctx, pool), "no bulk reconcile below the floor")
}

// TestTrustConfigDefaults documents the D-M6-8 graduation thresholds.
func TestTrustConfigDefaults(t *testing.T) {
	c := DefaultTrustConfig()
	require.Equal(t, 7*24*time.Hour, c.MinAge)
	require.Equal(t, int64(10), c.MinPassedAudits)
	require.Equal(t, int64(5), c.MinAckedXfers)
	require.InDelta(t, 0.95, c.GraduateRep, 0.0001)
}
