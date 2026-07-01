package possession

import (
	"context"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/db/gen"
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

// TestApplyTrustGraduatesEligibleNode verifies acceptance criterion #8 positive
// path (D-M6-14, D-M6-15 #8): a probationary node that satisfies all graduation
// conditions (age >= MinAge, score >= GraduateRep, passed >= MinPassedAudits,
// xfers >= MinAckedXfers, no review marker) is promoted to "trusted" by applyTrust.
func TestApplyTrustGraduatesEligibleNode(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	// Small thresholds so a single audit + pin satisfies the bars.
	tc := TrustConfig{
		MinAge:          1 * time.Hour,
		MinPassedAudits: 1,
		MinAckedXfers:   1,
		GraduateRep:     0.95,
	}
	a := NewAuditor(pool, notify.NoopNotifier{}, tc)

	node := seedNode(t, ctx, pool, 0.96, "probationary")

	// Set epoch 2 h ago — well past the 1 h MinAge gate.
	_, err := pool.Exec(ctx,
		`UPDATE nodes SET trust_epoch_started_at = now() - interval '2 hours' WHERE id = $1::uuid`, node)
	require.NoError(t, err)

	// Seed one passed audit with decided_at after the epoch.
	cid := seedBlob(t, ctx, pool)
	_, err = pool.Exec(ctx, `
		INSERT INTO pin_audits (id, blob_cid, node_id, challenge_kind, nonce, deadline, result, decided_at)
		VALUES (gen_random_uuid(), $1, $2::uuid, 'block_hash', 'nonce-g', now() + interval '30 seconds',
		        'pass'::audit_result, now())
	`, cid, node)
	require.NoError(t, err)

	// Seed one acked transfer after the epoch (seedAckedPin uses acked_at=now()).
	_, _ = seedAckedPin(t, ctx, pool, cid, node)

	nodePg, err := pgUUID(node)
	require.NoError(t, err)

	q := gen.New(pool)
	require.NoError(t, a.applyTrust(ctx, q, nodePg, 0.96, 0.5))

	require.Equal(t, "trusted", trustState(t, ctx, pool, node),
		"eligible probationary node must graduate to trusted")
}

// TestApplyTrustReviewGateBlocksGraduation verifies acceptance criterion #8 gate
// (D-M6-14, D-M6-15 #8): an otherwise-eligible node with trust_review_required_at
// set must NOT auto-graduate until an operator clears the marker.
func TestApplyTrustReviewGateBlocksGraduation(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	tc := TrustConfig{
		MinAge:          1 * time.Hour,
		MinPassedAudits: 1,
		MinAckedXfers:   1,
		GraduateRep:     0.95,
	}
	a := NewAuditor(pool, notify.NoopNotifier{}, tc)

	node := seedNode(t, ctx, pool, 0.96, "probationary")

	// Set epoch 2 h ago — satisfies the age gate.
	_, err := pool.Exec(ctx,
		`UPDATE nodes SET trust_epoch_started_at = now() - interval '2 hours' WHERE id = $1::uuid`, node)
	require.NoError(t, err)

	// Seed a pass audit and an acked transfer — all numeric conditions satisfied.
	cid := seedBlob(t, ctx, pool)
	_, err = pool.Exec(ctx, `
		INSERT INTO pin_audits (id, blob_cid, node_id, challenge_kind, nonce, deadline, result, decided_at)
		VALUES (gen_random_uuid(), $1, $2::uuid, 'block_hash', 'nonce-g2', now() + interval '30 seconds',
		        'pass'::audit_result, now())
	`, cid, node)
	require.NoError(t, err)
	_, _ = seedAckedPin(t, ctx, pool, cid, node)

	// Set the review gate — this must block graduation regardless of other conditions.
	_, err = pool.Exec(ctx,
		`UPDATE nodes SET trust_review_required_at = now(), trust_review_reason = 'test_gate' WHERE id = $1::uuid`, node)
	require.NoError(t, err)

	nodePg, err := pgUUID(node)
	require.NoError(t, err)

	q := gen.New(pool)
	require.NoError(t, a.applyTrust(ctx, q, nodePg, 0.96, 0.5))

	require.Equal(t, "probationary", trustState(t, ctx, pool, node),
		"review gate must block graduation even when all numeric conditions pass")
}

// TestRecordMismatchAdvancesEpoch verifies acceptance criterion #9 (D-M6-14,
// D-M6-15 #9): a hash-mismatch audit resets trust_epoch_started_at to now(),
// so graduation evidence must accrue afresh after any lying-donor event.
func TestRecordMismatchAdvancesEpoch(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	a := NewAuditor(pool, &recordingNotifier{}, testTrustConfig())

	node := seedNode(t, ctx, pool, 0.80, "probationary")

	// Pin the epoch well in the past so the advance is unambiguous.
	oldEpoch := time.Now().Add(-24 * time.Hour)
	_, err := pool.Exec(ctx,
		`UPDATE nodes SET trust_epoch_started_at = $2 WHERE id = $1::uuid`, node, oldEpoch)
	require.NoError(t, err)

	cid := seedBlob(t, ctx, pool)
	aid, generation := seedAckedPin(t, ctx, pool, cid, node)
	auditID := seedChallenge(t, ctx, pool, cid, node)

	before := time.Now()
	res := DispatchResult{Outcome: OutcomeFailMismatch, ReceivedAt: time.Now()}
	require.NoError(t, a.Record(ctx, auditTarget(auditID, node, cid, aid, generation), res, 0.5))

	var newEpoch time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT trust_epoch_started_at FROM nodes WHERE id=$1::uuid`, node).Scan(&newEpoch))

	require.True(t, newEpoch.After(oldEpoch),
		"trust_epoch_started_at must advance beyond the seeded old value after mismatch")
	require.False(t, newEpoch.Before(before),
		"new epoch must be at or after the mismatch-record time")
}
