package possession

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/notify"
	"github.com/stretchr/testify/require"
)

// recordingNotifier captures post-commit federation events for assertions.
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

func testTrustConfig() TrustConfig {
	return TrustConfig{
		MinAge:          7 * 24 * time.Hour,
		MinPassedAudits: 10,
		MinAckedXfers:   5,
		GraduateRep:     0.95,
	}
}

// seedNode inserts an active node with the given reputation and trust state and
// returns its id. trust_epoch_started_at defaults to now() (well inside MinAge).
func seedNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rep float64, trustState string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(ctx, `
		INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint,
		                   capacity_bytes, bandwidth_budget_bytes_per_day, policy_filters,
		                   status, reputation_score, trust_state)
		VALUES ($1::uuid, $2, $2, 10000000, 10000000, '{}', 'active', $3, $4)
	`, id, id, rep, trustState)
	require.NoError(t, err)
	return id
}

func seedBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	cid := "cid-" + uuid.NewString()
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product)
		VALUES ($1, 'image/jpeg', 500, 'active', 'image')
	`, cid)
	require.NoError(t, err)
	return cid
}

// seedAckedPin inserts an acked pin_assignment and returns (assignment_id, generation).
func seedAckedPin(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid, nodeID string) (string, int64) {
	t.Helper()
	var aid string
	var generation int64
	err := pool.QueryRow(ctx, `
		INSERT INTO pin_assignments (cid, node_id, state, acked_at)
		VALUES ($1, $2::uuid, 'acked', now())
		RETURNING assignment_id, generation
	`, cid, nodeID).Scan(&aid, &generation)
	require.NoError(t, err)
	return aid, generation
}

// seedChallenge inserts a pin_audits row with result NULL (pre-dispatch) so
// RecordAuditOutcome's "WHERE result IS NULL" UPDATE has a row to resolve.
func seedChallenge(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid, nodeID string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(ctx, `
		INSERT INTO pin_audits (id, blob_cid, node_id, challenge_kind, nonce, deadline)
		VALUES ($1::uuid, $2, $3::uuid, 'block_hash', 'nonce-x', now() + interval '30 seconds')
	`, id, cid, nodeID)
	require.NoError(t, err)
	return id
}

func auditTarget(auditID, nodeID, cid, assignID string, generation int64) AuditTarget {
	return AuditTarget{
		AuditID:      auditID,
		NodeID:       nodeID,
		BlobCID:      cid,
		BlockCID:     "blk-" + cid,
		AssignmentID: assignID,
		Nonce:        "nonce-x",
		Generation:   generation,
		BlockIndex:   0,
		BlockSize:    11,
		Deadline:     time.Now().Add(30 * time.Second),
	}
}

// --- read helpers -----------------------------------------------------------

func repScore(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID string) float64 {
	t.Helper()
	var s float32
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT reputation_score FROM nodes WHERE id=$1::uuid`, nodeID).Scan(&s))
	return float64(s)
}

func pinState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid, nodeID string) string {
	t.Helper()
	var st string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT state FROM pin_assignments WHERE cid=$1 AND node_id=$2::uuid`, cid, nodeID).Scan(&st))
	return st
}

func auditResult(t *testing.T, ctx context.Context, pool *pgxpool.Pool, auditID string) *string {
	t.Helper()
	var r *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT result::text FROM pin_audits WHERE id=$1::uuid`, auditID).Scan(&r))
	return r
}

func auditError(t *testing.T, ctx context.Context, pool *pgxpool.Pool, auditID string) *string {
	t.Helper()
	var e *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT error FROM pin_audits WHERE id=$1::uuid`, auditID).Scan(&e))
	return e
}

func auditReceivedValid(t *testing.T, ctx context.Context, pool *pgxpool.Pool, auditID string) bool {
	t.Helper()
	var ts pgtype.Timestamptz
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT received_at FROM pin_audits WHERE id=$1::uuid`, auditID).Scan(&ts))
	return ts.Valid
}

func auditTranscript(t *testing.T, ctx context.Context, pool *pgxpool.Pool, auditID string) []byte {
	t.Helper()
	var b []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT transcript_hash FROM pin_audits WHERE id=$1::uuid`, auditID).Scan(&b))
	return b
}

func reconcileCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blob_replication_reconcile_queue WHERE cid=$1`, cid).Scan(&n))
	return n
}

func reconcileReasonFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string) string {
	t.Helper()
	var r string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT reason FROM blob_replication_reconcile_queue WHERE cid=$1`, cid).Scan(&r))
	return r
}

func reconcileTotal(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blob_replication_reconcile_queue`).Scan(&n))
	return n
}

func trustState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID string) string {
	t.Helper()
	var st string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT trust_state FROM nodes WHERE id=$1::uuid`, nodeID).Scan(&st))
	return st
}

// --- tests ------------------------------------------------------------------

func TestRecordPassDriftsReputationUp(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	a := NewAuditor(pool, notify.NoopNotifier{}, testTrustConfig())

	node := seedNode(t, ctx, pool, 0.90, "probationary")
	cid := seedBlob(t, ctx, pool)
	aid, generation := seedAckedPin(t, ctx, pool, cid, node)
	auditID := seedChallenge(t, ctx, pool, cid, node)

	res := DispatchResult{Outcome: OutcomePass, Bytes: []byte("hello-block"), ReceivedAt: time.Now(), LatencyMS: 5}
	require.NoError(t, a.Record(ctx, auditTarget(auditID, node, cid, aid, generation), res, 0.5))

	require.InDelta(t, 0.91, repScore(t, ctx, pool, node), 0.0005)
	require.Equal(t, "acked", pinState(t, ctx, pool, cid, node))
	if r := auditResult(t, ctx, pool, auditID); assertResult(t, r, "pass") {
		require.True(t, auditReceivedValid(t, ctx, pool, auditID), "received_at must be set on pass")
		require.NotEmpty(t, auditTranscript(t, ctx, pool, auditID), "transcript_hash must be non-null on pass")
	}
	require.Equal(t, 0, reconcileTotal(t, ctx, pool))
}

func TestRecordDeadlineSoftFailKeepsPin(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	a := NewAuditor(pool, notify.NoopNotifier{}, testTrustConfig())

	node := seedNode(t, ctx, pool, 0.80, "probationary")
	cid := seedBlob(t, ctx, pool)
	aid, generation := seedAckedPin(t, ctx, pool, cid, node)
	auditID := seedChallenge(t, ctx, pool, cid, node)

	res := DispatchResult{Outcome: OutcomeFailDeadline}
	require.NoError(t, a.Record(ctx, auditTarget(auditID, node, cid, aid, generation), res, 0.5))

	require.InDelta(t, 0.76, repScore(t, ctx, pool, node), 0.0005)
	require.Equal(t, "acked", pinState(t, ctx, pool, cid, node))
	assertResult(t, auditResult(t, ctx, pool, auditID), "fail")
	require.Equal(t, 0, reconcileTotal(t, ctx, pool), "soft fail must not enqueue reconcile")
}

func TestRecordNotPresentHardFailsPin(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	a := NewAuditor(pool, notify.NoopNotifier{}, testTrustConfig())

	node := seedNode(t, ctx, pool, 0.80, "probationary")
	cid := seedBlob(t, ctx, pool)
	aid, generation := seedAckedPin(t, ctx, pool, cid, node)
	auditID := seedChallenge(t, ctx, pool, cid, node)

	res := DispatchResult{Outcome: OutcomeFailNotPresent, ReceivedAt: time.Now()}
	require.NoError(t, a.Record(ctx, auditTarget(auditID, node, cid, aid, generation), res, 0.5))

	require.InDelta(t, 0.40, repScore(t, ctx, pool, node), 0.0005)
	require.Equal(t, "failed", pinState(t, ctx, pool, cid, node))
	assertResult(t, auditResult(t, ctx, pool, auditID), "fail")
	require.Equal(t, 1, reconcileCount(t, ctx, pool, cid))
	require.Equal(t, "audit_not_present", reconcileReasonFor(t, ctx, pool, cid))
}

func TestRecordMismatchZeroesAndSuspects(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	rec := &recordingNotifier{}
	a := NewAuditor(pool, rec, testTrustConfig())

	node := seedNode(t, ctx, pool, 0.80, "probationary")
	cid := seedBlob(t, ctx, pool)
	aid, generation := seedAckedPin(t, ctx, pool, cid, node)
	auditID := seedChallenge(t, ctx, pool, cid, node)

	res := DispatchResult{Outcome: OutcomeFailMismatch, ReceivedAt: time.Now()}
	require.NoError(t, a.Record(ctx, auditTarget(auditID, node, cid, aid, generation), res, 0.5))

	require.InDelta(t, 0.0, repScore(t, ctx, pool, node), 0.0005)
	require.Equal(t, "failed", pinState(t, ctx, pool, cid, node))
	require.Equal(t, 1, reconcileCount(t, ctx, pool, cid))
	require.Equal(t, "audit_mismatch", reconcileReasonFor(t, ctx, pool, cid))

	var reviewValid bool
	var reviewReason *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT trust_review_required_at IS NOT NULL, trust_review_reason FROM nodes WHERE id=$1::uuid`, node).
		Scan(&reviewValid, &reviewReason))
	require.True(t, reviewValid, "trust_review_required_at must be set on mismatch")
	require.NotNil(t, reviewReason)
	require.Equal(t, "hash_mismatch", *reviewReason)

	require.Equal(t, 1, rec.count("federation.node_suspect"), "must emit federation.node_suspect post-commit")
}

func TestRecordStaleChallengeSkips(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	a := NewAuditor(pool, notify.NoopNotifier{}, testTrustConfig())

	node := seedNode(t, ctx, pool, 0.80, "probationary")
	cid := seedBlob(t, ctx, pool)
	aid, generation := seedAckedPin(t, ctx, pool, cid, node)
	auditID := seedChallenge(t, ctx, pool, cid, node)

	// Audit a generation that is no longer the live one -> RevalidateAuditPin=false.
	res := DispatchResult{Outcome: OutcomePass, Bytes: []byte("hello-block"), ReceivedAt: time.Now(), LatencyMS: 5}
	require.NoError(t, a.Record(ctx, auditTarget(auditID, node, cid, aid, generation+99), res, 0.5))

	require.InDelta(t, 0.80, repScore(t, ctx, pool, node), 0.0005, "stale challenge must not move reputation")
	require.Equal(t, "acked", pinState(t, ctx, pool, cid, node), "stale challenge must leave the pin untouched")
	assertResult(t, auditResult(t, ctx, pool, auditID), "skip")
	if e := auditError(t, ctx, pool, auditID); assertNonNil(t, e) {
		require.Equal(t, "stale_challenge", *e)
	}
}

func TestRecordUnreachableIsSkipNoMovement(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	a := NewAuditor(pool, notify.NoopNotifier{}, testTrustConfig())

	node := seedNode(t, ctx, pool, 0.80, "probationary")
	cid := seedBlob(t, ctx, pool)
	aid, generation := seedAckedPin(t, ctx, pool, cid, node)
	auditID := seedChallenge(t, ctx, pool, cid, node)

	res := DispatchResult{Outcome: OutcomeSkipUnreachable}
	require.NoError(t, a.Record(ctx, auditTarget(auditID, node, cid, aid, generation), res, 0.5))

	require.InDelta(t, 0.80, repScore(t, ctx, pool, node), 0.0005, "unreachable skip must not move reputation")
	require.Equal(t, "acked", pinState(t, ctx, pool, cid, node))
	assertResult(t, auditResult(t, ctx, pool, auditID), "skip")
	if e := auditError(t, ctx, pool, auditID); assertNonNil(t, e) {
		require.Equal(t, "unreachable", *e)
	}
	require.False(t, auditReceivedValid(t, ctx, pool, auditID), "unreachable donor has NULL received_at (D10)")
}

func TestRecordOutcomeReplayDoesNotOverwrite(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	a := NewAuditor(pool, notify.NoopNotifier{}, testTrustConfig())

	node := seedNode(t, ctx, pool, 0.90, "probationary")
	cid := seedBlob(t, ctx, pool)
	aid, generation := seedAckedPin(t, ctx, pool, cid, node)
	auditID := seedChallenge(t, ctx, pool, cid, node)

	target := auditTarget(auditID, node, cid, aid, generation)
	res := DispatchResult{Outcome: OutcomePass, Bytes: []byte("hello-block"), ReceivedAt: time.Now(), LatencyMS: 5}

	require.NoError(t, a.Record(ctx, target, res, 0.5))
	require.InDelta(t, 0.91, repScore(t, ctx, pool, node), 0.0005)

	// Replay the exact same outcome: the result IS NULL UPDATE now matches 0 rows,
	// so neither the recorded outcome nor the reputation may move again.
	require.NoError(t, a.Record(ctx, target, res, 0.5))
	require.InDelta(t, 0.91, repScore(t, ctx, pool, node), 0.0005, "replay must not move reputation again")
	assertResult(t, auditResult(t, ctx, pool, auditID), "pass")
}

// assertResult fails the test if got is nil or != want, and returns true when it matched.
func assertResult(t *testing.T, got *string, want string) bool {
	t.Helper()
	if !assertNonNil(t, got) {
		return false
	}
	require.Equal(t, want, *got)
	return *got == want
}

func assertNonNil(t *testing.T, got *string) bool {
	t.Helper()
	require.NotNil(t, got, "expected a non-null value")
	return got != nil
}
