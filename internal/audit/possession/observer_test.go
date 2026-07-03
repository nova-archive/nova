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

// recObserver records the P2-M7 (D-M7-1) observability hook calls.
type recObserver struct {
	transitions []string // "from>to:reason"
	moves       []string
	latencies   []float64
}

func (r *recObserver) TrustTransition(from, to, reason string) {
	r.transitions = append(r.transitions, from+">"+to+":"+reason)
}
func (r *recObserver) ReputationMoved(direction string) { r.moves = append(r.moves, direction) }
func (r *recObserver) AuditLatency(sec float64)         { r.latencies = append(r.latencies, sec) }

func TestAuditorObserverSeesReputationAndLatency(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	a := NewAuditor(pool, notify.NoopNotifier{}, testTrustConfig())
	obs := &recObserver{}
	a.SetObserver(obs)

	node := seedNode(t, ctx, pool, 0.90, "probationary")
	cid := seedBlob(t, ctx, pool)
	aid, generation := seedAckedPin(t, ctx, pool, cid, node)
	auditID := seedChallenge(t, ctx, pool, cid, node)

	res := DispatchResult{Outcome: OutcomePass, Bytes: []byte("hello-block"), ReceivedAt: time.Now(), LatencyMS: 5}
	require.NoError(t, a.Record(ctx, auditTarget(auditID, node, cid, aid, generation), res, 0.5))

	require.Equal(t, []string{"up"}, obs.moves, "a pass drifts reputation up")
	require.Len(t, obs.latencies, 1)
	require.Greater(t, obs.latencies[0], 0.0)
}

func TestAuditorObserverSeesTrustTransitions(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	tc := TrustConfig{MinAge: time.Hour, MinPassedAudits: 1, MinAckedXfers: 1, GraduateRep: 0.95}
	a := NewAuditor(pool, notify.NoopNotifier{}, tc)
	obs := &recObserver{}
	a.SetObserver(obs)
	q := gen.New(pool)

	// Graduation (mirror TestApplyTrustGraduatesEligibleNode's fixture).
	node := seedNode(t, ctx, pool, 0.96, "probationary")
	_, err := pool.Exec(ctx,
		`UPDATE nodes SET trust_epoch_started_at = now() - interval '2 hours' WHERE id = $1::uuid`, node)
	require.NoError(t, err)
	cid := seedBlob(t, ctx, pool)
	_, err = pool.Exec(ctx, `
		INSERT INTO pin_audits (id, blob_cid, node_id, challenge_kind, nonce, deadline, result, decided_at)
		VALUES (gen_random_uuid(), $1, $2::uuid, 'block_hash', 'nonce-g', now() + interval '30 seconds',
		        'pass'::audit_result, now())`, cid, node)
	require.NoError(t, err)
	_, _ = seedAckedPin(t, ctx, pool, cid, node)
	nodePg, err := pgUUID(node)
	require.NoError(t, err)
	require.NoError(t, a.applyTrust(ctx, q, nodePg, 0.96, 0.5))
	require.Equal(t, []string{"probationary>trusted:graduated"}, obs.transitions)

	// Demotion: trusted node below the floor.
	demoted := seedNode(t, ctx, pool, 0.3, "trusted")
	demotedPg, err := pgUUID(demoted)
	require.NoError(t, err)
	require.NoError(t, a.applyTrust(ctx, q, demotedPg, 0.3, 0.5))
	require.Equal(t, "trusted>probationary:below_floor", obs.transitions[len(obs.transitions)-1])
}
