package possession

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/notify"
	"github.com/stretchr/testify/require"
)

// fakeChallenger is a test double for the challenger interface.
type fakeChallenger struct {
	mu     sync.Mutex
	calls  int
	result DispatchResult
}

func (f *fakeChallenger) Challenge(_ context.Context, _ string, _ wire.AuditChallenge) (DispatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.result, nil
}

func (f *fakeChallenger) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// seedAuditableNode inserts a node that passes all SelectDueAuditNodes filters.
// status='active', assignment_sync_state='current', advertised_capabilities,
// source_nebula_addr set. Returns the node id string.
func seedAuditableNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rep float64, trustState string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(ctx, `
		INSERT INTO nodes (
			id, nebula_cert_fingerprint, federation_cert_fingerprint,
			capacity_bytes, bandwidth_budget_bytes_per_day, policy_filters,
			status, reputation_score, trust_state,
			assignment_sync_state, advertised_capabilities, source_nebula_addr
		) VALUES (
			$1::uuid, $2, $2,
			10000000, 10000000, '{}',
			'active', $3, $4,
			'current', '{audit-block-hash/v1}', '10.0.0.9:9200'
		)
	`, id, id, rep, trustState)
	require.NoError(t, err)
	return id
}

// seedBlobManifest inserts a blob_manifests row required by SelectAckedPinForAudit.
func seedBlobManifest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO blob_manifests (cid, cid_version, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		VALUES ($1, 1, 'sha2-256', 'dag-pb', 'size-262144', 500, 550, 1)
	`, cid)
	require.NoError(t, err)
}

// seedBlobBlock inserts a blob_blocks row with the given block_size.
func seedBlobBlock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, blobCID string, blockSize int) {
	t.Helper()
	blockCID := "blk-" + uuid.NewString()
	_, err := pool.Exec(ctx, `
		INSERT INTO blob_blocks (blob_cid, block_cid, block_index, block_size)
		VALUES ($1, $2, 0, $3)
	`, blobCID, blockCID, blockSize)
	require.NoError(t, err)
}

// seedAckedPinAt inserts a pin_assignment in 'acked' state with the given acked_at.
func seedAckedPinAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid, nodeID string, ackedAt time.Time) (string, int64) {
	t.Helper()
	var aid string
	var generation int64
	err := pool.QueryRow(ctx, `
		INSERT INTO pin_assignments (cid, node_id, state, acked_at)
		VALUES ($1, $2::uuid, 'acked', $3)
		RETURNING assignment_id, generation
	`, cid, nodeID, ackedAt).Scan(&aid, &generation)
	require.NoError(t, err)
	return aid, generation
}

// countPinAudits returns the number of pin_audits rows for the given node.
func countPinAudits(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pin_audits WHERE node_id = $1::uuid`, nodeID).Scan(&n))
	return n
}

// defaultSchedulerCfg returns a SchedulerConfig suitable for tests.
func defaultSchedulerCfg() SchedulerConfig {
	return SchedulerConfig{
		NewAckWindow:      1 * time.Hour,
		FastLaneQuota:     10,
		NodesPerTick:      10,
		MaxBlockBytes:     1 << 20,
		Deadline:          5 * time.Second,
		ReputationFloor:   0.5,
		StaleGraceSeconds: 0,
		BaseInterval:      0, // 0 means always due (zero interval elapsed)
	}
}

// TestSchedulerStartupReconcilesStaleAudits: a pin_audits row with result IS NULL
// and deadline in the past must be set to result='skip' by ReconcileOnStartup.
func TestSchedulerStartupReconcilesStaleAudits(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	node := seedNode(t, ctx, pool, 0.8, "trusted")
	cid := seedBlob(t, ctx, pool)
	_, _ = seedAckedPin(t, ctx, pool, cid, node)

	// Insert a pin_audits row with result IS NULL and deadline well in the past.
	staleAuditID := uuid.NewString()
	_, err := pool.Exec(ctx, `
		INSERT INTO pin_audits (id, blob_cid, node_id, challenge_kind, nonce, deadline, challenged_at)
		VALUES ($1::uuid, $2, $3::uuid, 'block_hash', 'nonce-stale',
		        now() - interval '10 minutes', now() - interval '10 minutes')
	`, staleAuditID, cid, node)
	require.NoError(t, err)

	// Verify result IS NULL before reconcile.
	r := auditResult(t, ctx, pool, staleAuditID)
	require.Nil(t, r, "result must be NULL before ReconcileOnStartup")

	fc := &fakeChallenger{result: DispatchResult{Outcome: OutcomePass}}
	auditor := NewAuditor(pool, notify.NoopNotifier{}, DefaultTrustConfig())
	cfg := defaultSchedulerCfg()
	cfg.StaleGraceSeconds = 0 // mark all past-deadline NULL rows immediately
	s := NewScheduler(pool, fc, auditor, cfg)

	require.NoError(t, s.ReconcileOnStartup(ctx))

	// After reconcile, the stale row must have result='skip'.
	got := auditResult(t, ctx, pool, staleAuditID)
	require.NotNil(t, got, "result must be set after ReconcileOnStartup")
	require.Equal(t, "skip", *got)
}

// TestSchedulerFastLaneHasQuota: with FastLaneQuota=2 and 5 newly-acked pins,
// the fake challenger must be invoked at most 2 times per tick.
func TestSchedulerFastLaneHasQuota(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	// Seed 5 challengeable nodes each with one newly-acked pin.
	for i := 0; i < 5; i++ {
		node := seedAuditableNode(t, ctx, pool, 0.8, "trusted")
		cid := seedBlob(t, ctx, pool)
		seedBlobManifest(t, ctx, pool, cid)
		seedBlobBlock(t, ctx, pool, cid, 512)
		_, _ = seedAckedPinAt(t, ctx, pool, cid, node, time.Now())
	}

	fc := &fakeChallenger{result: DispatchResult{Outcome: OutcomePass, Bytes: []byte("x"), ReceivedAt: time.Now()}}
	auditor := NewAuditor(pool, notify.NoopNotifier{}, DefaultTrustConfig())
	cfg := defaultSchedulerCfg()
	cfg.FastLaneQuota = 2
	cfg.NodesPerTick = 0 // disable baseline so only fast-lane runs
	s := NewScheduler(pool, fc, auditor, cfg)

	s.runOnce(ctx)

	require.LessOrEqual(t, fc.callCount(), 2, "fast-lane must not exceed quota per tick")
}

// TestSchedulerSkipsOverCapBlocks: if the only blob_blocks row has
// block_size > MaxBlockBytes, auditOne finds no eligible block and issues
// no challenge.
func TestSchedulerSkipsOverCapBlocks(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	node := seedAuditableNode(t, ctx, pool, 0.8, "trusted")
	cid := seedBlob(t, ctx, pool)
	seedBlobManifest(t, ctx, pool, cid)
	// Block size (2048) exceeds MaxBlockBytes (1024).
	seedBlobBlock(t, ctx, pool, cid, 2048)
	_, _ = seedAckedPinAt(t, ctx, pool, cid, node, time.Now().Add(-2*time.Hour))

	fc := &fakeChallenger{result: DispatchResult{Outcome: OutcomePass}}
	auditor := NewAuditor(pool, notify.NoopNotifier{}, DefaultTrustConfig())
	cfg := defaultSchedulerCfg()
	cfg.MaxBlockBytes = 1024
	cfg.FastLaneQuota = 0 // disable fast lane
	cfg.NodesPerTick = 10 // enable baseline
	cfg.BaseInterval = 0  // node is always due
	s := NewScheduler(pool, fc, auditor, cfg)

	s.runOnce(ctx)

	require.Equal(t, 0, fc.callCount(), "no challenge must be dispatched when all blocks exceed cap")
	require.Equal(t, 0, countPinAudits(t, ctx, pool, node), "no pin_audits row must be inserted")
}

// TestSchedulerOneTickChallengesDueNode: a fully-seeded due node with an
// in-cap block and a fake dispatcher returning OutcomePass must result in a
// pin_audits row with result='pass'.
func TestSchedulerOneTickChallengesDueNode(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	node := seedAuditableNode(t, ctx, pool, 0.8, "trusted")
	cid := seedBlob(t, ctx, pool)
	seedBlobManifest(t, ctx, pool, cid)
	seedBlobBlock(t, ctx, pool, cid, 512) // well within 1 MiB cap
	_, _ = seedAckedPinAt(t, ctx, pool, cid, node, time.Now().Add(-2*time.Hour))

	fc := &fakeChallenger{result: DispatchResult{
		Outcome:    OutcomePass,
		Bytes:      []byte("x"),
		ReceivedAt: time.Now(),
	}}
	auditor := NewAuditor(pool, notify.NoopNotifier{}, DefaultTrustConfig())
	cfg := defaultSchedulerCfg()
	cfg.FastLaneQuota = 0 // disable fast lane; drive through baseline only
	cfg.NodesPerTick = 10
	cfg.BaseInterval = 0 // always due
	s := NewScheduler(pool, fc, auditor, cfg)

	s.runOnce(ctx)

	require.Equal(t, 1, fc.callCount(), "exactly one challenge must be dispatched")

	// Assert a pin_audits row with result='pass' was written for the node.
	var result string
	err := pool.QueryRow(ctx,
		`SELECT result::text FROM pin_audits WHERE node_id = $1::uuid`, node).Scan(&result)
	require.NoError(t, err, "a pin_audits row must exist for the node")
	require.Equal(t, "pass", result)
}
