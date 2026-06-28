package storage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// reconcilerHook is a test-only WriteHook that records OnCommitted calls.
// Analyze is a no-op (the reconciler never calls it).
type reconcilerHook struct {
	mu        sync.Mutex
	committed []CommittedRef
}

func (h *reconcilerHook) Analyze(_ context.Context, _ PutContext, _ []byte) (AnalyzeResult, error) {
	return AnalyzeResult{}, nil
}

func (h *reconcilerHook) OnCommitted(_ context.Context, ref CommittedRef) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.committed = append(h.committed, ref)
}

func (h *reconcilerHook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.committed)
}

func (h *reconcilerHook) refs() []CommittedRef {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]CommittedRef, len(h.committed))
	copy(out, h.committed)
	return out
}

// seedReconcilerBlob inserts a blobs + blob_manifests row for the given cid.
// The blobs row satisfies ResolveEffectiveVisibility; the blob_manifests row
// satisfies GetBlobSize (reconciler.commit reads envelope_size).
func seedReconcilerBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) {
	t.Helper()
	const envelopeSize = 512
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ($1, 'application/octet-stream', $2, 'active', 'raw', 2)
		ON CONFLICT (cid) DO NOTHING
	`, cidStr, envelopeSize)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		VALUES ($1, 'sha2-256', 'raw', 'size-262144', $2, $2, 1)
		ON CONFLICT (cid) DO NOTHING
	`, cidStr, envelopeSize)
	require.NoError(t, err)
}

// seedStagingState upserts a staging blob_storage_state row with durability_class='important'.
func seedStagingState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) {
	t.Helper()
	err := gen.New(pool).UpsertStorageStateStaging(ctx, gen.UpsertStorageStateStagingParams{
		Cid:             cidStr,
		DurabilityClass: "important",
	})
	require.NoError(t, err)
}

// seedSourceableNode inserts a node satisfying CountSourceableHolders predicates:
// status=active, trust_state=trusted, read-source/v1 capability, non-empty
// source_nebula_addr, last_seen_at=now() (fresh within any stale window).
func seedSourceableNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID uuid.UUID) {
	t.Helper()
	idStr := nodeID.String()
	_, err := pool.Exec(ctx, `
		INSERT INTO nodes (
			id, nebula_cert_fingerprint, federation_cert_fingerprint,
			capacity_bytes, bandwidth_budget_bytes_per_day, policy_filters,
			status, trust_state, selected_protocol,
			advertised_capabilities, required_capabilities, reputation_score,
			source_nebula_addr, last_seen_at
		) VALUES (
			$1, 'neb-'||$2, 'fed-'||$2,
			1000000000, 1000000000, '{}',
			'active', 'trusted', 'blob-transfer/v1',
			ARRAY['read-source/v1'], ARRAY[]::text[], 0.9,
			'127.0.0.1:4242', now()
		)
	`, pgtype.UUID{Bytes: nodeID, Valid: true}, idStr)
	require.NoError(t, err)
}

// seedAckedHolder inserts a pin_assignments row with state='acked' linking
// nodeID to cidStr, satisfying CountSourceableHolders' JOIN predicate.
func seedAckedHolder(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string, nodeID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO pin_assignments (cid, node_id, state)
		VALUES ($1, $2, 'acked')
	`, cidStr, pgtype.UUID{Bytes: nodeID, Valid: true})
	require.NoError(t, err)
}

// newReconcilerPair builds a Service + Reconciler for reconciler tests.
// backend and ks are nil — the reconciler only touches q + hook + gate.
func newReconcilerPair(pool *pgxpool.Pool, h *reconcilerHook, failAfter time.Duration) (*Service, *Reconciler) {
	svc := NewService(pool, nil, nil,
		WithCommitGate(CommitGateConfig{
			RequireQuorum: true,
			Quorum:        config.ReplicationFactor{Important: 2, Normal: 2, Cache: 1},
			FailAfter:     failAfter,
			StaleSeconds:  3600,
		}),
		WithProductHook(h),
	)
	return svc, NewReconciler(svc)
}

// TestReconcilerCommitsOnQuorumThenFiresHook verifies that reconcileOnce
// transitions a staging blob to 'committed' and fires the hook exactly once
// when the sourceable-holder count meets the Important quorum (2).
func TestReconcilerCommitsOnQuorumThenFiresHook(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	payload := []byte("reconciler-commit-quorum-test")
	cidStr := mkRawCID(t, payload)

	seedReconcilerBlob(t, ctx, pool, cidStr)
	seedStagingState(t, ctx, pool, cidStr)

	// Seed 2 acked sourceable nodes — quorum Important=2 is met.
	node1, node2 := uuid.New(), uuid.New()
	seedSourceableNode(t, ctx, pool, node1)
	seedSourceableNode(t, ctx, pool, node2)
	seedAckedHolder(t, ctx, pool, cidStr, node1)
	seedAckedHolder(t, ctx, pool, cidStr, node2)

	h := &reconcilerHook{}
	_, r := newReconcilerPair(pool, h, 1*time.Hour)
	require.NotNil(t, r, "NewReconciler must return non-nil when RequireQuorum=true")

	r.reconcileOnce(ctx)

	// Assert: commit_state == 'committed'.
	cs, err := gen.New(pool).GetCommitState(ctx, cidStr)
	require.NoError(t, err)
	require.Equal(t, gen.BlobCommitStateCommitted, cs,
		"blob must be committed once quorum is met")

	// Assert: hook fired exactly once with the right CID and Product.
	require.Equal(t, 1, h.count(), "OnCommitted must fire exactly once")
	refs := h.refs()
	require.Equal(t, cidStr, refs[0].CID, "fired CID must match")
	require.Equal(t, "raw", refs[0].Product, "fired Product must match the blobs row")
}

// TestReconcilerMarksFailed verifies that reconcileOnce marks a staging blob
// 'failed' once it has been staging longer than FailAfter with quorum unmet,
// and does NOT fire the OnCommitted hook.
func TestReconcilerMarksFailed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	payload := []byte("reconciler-mark-failed-test")
	cidStr := mkRawCID(t, payload)

	seedReconcilerBlob(t, ctx, pool, cidStr)
	seedStagingState(t, ctx, pool, cidStr)

	// Backdate updated_at by 2 hours — past FailAfter of 1 hour.
	_, err := pool.Exec(ctx,
		`UPDATE blob_storage_state SET updated_at = now() - interval '2 hours' WHERE cid = $1`,
		cidStr)
	require.NoError(t, err)

	// No holders seeded — quorum is 0 of 2 required.

	h := &reconcilerHook{}
	_, r := newReconcilerPair(pool, h, 1*time.Hour)
	require.NotNil(t, r)

	r.reconcileOnce(ctx)

	// Assert: commit_state == 'failed'.
	cs, err := gen.New(pool).GetCommitState(ctx, cidStr)
	require.NoError(t, err)
	require.Equal(t, gen.BlobCommitStateFailed, cs,
		"blob must be marked failed once FailAfter is exceeded with quorum unmet")

	// Assert: hook NOT fired.
	require.Equal(t, 0, h.count(), "OnCommitted must NOT fire for a failed blob")
}

// TestReconcilerIdempotent verifies that calling reconcileOnce twice on a
// quorum-met staging blob fires OnCommitted exactly once total: the
// MarkCommitted rows==1 guard prevents re-firing on subsequent passes.
func TestReconcilerIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	payload := []byte("reconciler-idempotent-test")
	cidStr := mkRawCID(t, payload)

	seedReconcilerBlob(t, ctx, pool, cidStr)
	seedStagingState(t, ctx, pool, cidStr)

	// Seed 2 acked sourceable holders — quorum met.
	node1, node2 := uuid.New(), uuid.New()
	seedSourceableNode(t, ctx, pool, node1)
	seedSourceableNode(t, ctx, pool, node2)
	seedAckedHolder(t, ctx, pool, cidStr, node1)
	seedAckedHolder(t, ctx, pool, cidStr, node2)

	h := &reconcilerHook{}
	_, r := newReconcilerPair(pool, h, 1*time.Hour)
	require.NotNil(t, r)

	// First pass: commits the blob and fires the hook once.
	r.reconcileOnce(ctx)
	require.Equal(t, 1, h.count(), "first pass must fire OnCommitted exactly once")

	cs, err := gen.New(pool).GetCommitState(ctx, cidStr)
	require.NoError(t, err)
	require.Equal(t, gen.BlobCommitStateCommitted, cs,
		"commit_state must be 'committed' after first pass")

	// Second pass: blob is already committed; MarkCommitted returns 0 rows,
	// so the hook must NOT fire again.
	r.reconcileOnce(ctx)
	require.Equal(t, 1, h.count(),
		"second pass must not re-fire OnCommitted (idempotency via rows==1 guard)")

	cs, err = gen.New(pool).GetCommitState(ctx, cidStr)
	require.NoError(t, err)
	require.Equal(t, gen.BlobCommitStateCommitted, cs,
		"commit_state must remain 'committed' after second pass")
}
