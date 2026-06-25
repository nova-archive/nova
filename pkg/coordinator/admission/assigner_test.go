package admission_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/pkg/coordinator/admission"
	"github.com/stretchr/testify/require"
)

// seedNode inserts a fully-wired node row. caps should include
// 'read-source/v1' for sourceable nodes; omit or vary to test filters.
func seedNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, reputation float32, lastFreeBytes *int64, caps []string, status, trustState, sourceAddr string) {
	t.Helper()
	var freeBytesArg pgtype.Int8
	if lastFreeBytes != nil {
		freeBytesArg = pgtype.Int8{Int64: *lastFreeBytes, Valid: true}
	}
	idStr := id.String()
	_, err := pool.Exec(ctx, `
		INSERT INTO nodes (
			id, nebula_cert_fingerprint, federation_cert_fingerprint,
			display_name, capacity_bytes, bandwidth_budget_bytes_per_day,
			policy_filters, status, trust_state, selected_protocol,
			advertised_capabilities, required_capabilities, reputation_score,
			source_nebula_addr, last_free_bytes
		) VALUES (
			$1, 'neb-'||$2, 'fed-'||$2,
			'node-'||$2, 1000000000, 1000000000,
			'{}', $3, $4, 'blob-transfer/v1',
			$5, ARRAY[]::text[], $6,
			$7, $8
		)
	`, pgtype.UUID{Bytes: id, Valid: true}, idStr, status, trustState, caps, reputation, sourceAddr, freeBytesArg)
	require.NoError(t, err)
}

// seedBlob inserts a blobs row + blob_manifests row with the given envelope_size.
// This is required by AssignPin which calls GetBlobSize (manifest) and
// the blobs FK from pin_assignments (blobs table).
func seedBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string, envelopeSize int64) {
	t.Helper()
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

// queryPinAssignmentCount returns the number of pin_assignments for the given cid.
func queryPinAssignmentCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) int {
	t.Helper()
	var count int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM pin_assignments WHERE cid = $1`, cidStr).Scan(&count)
	require.NoError(t, err)
	return count
}

// queryPinAssignmentNodeIDs returns the node_ids assigned for the given cid.
func queryPinAssignmentNodeIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) []uuid.UUID {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT node_id FROM pin_assignments WHERE cid = $1`, cidStr)
	require.NoError(t, err)
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var pg pgtype.UUID
		require.NoError(t, rows.Scan(&pg))
		ids = append(ids, uuid.UUID(pg.Bytes))
	}
	require.NoError(t, rows.Err())
	return ids
}

// seedChangeLogState ensures the federation_change_log_state row exists (required
// by AcquireChangeLogLock inside AssignPin).
func seedChangeLogState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO federation_change_log_state (id, pruned_through_seq) VALUES (true, 0) ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
}

func TestAssignerPicksRClassDonors(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedChangeLogState(t, ctx, pool)

	factor := config.ReplicationFactor{Important: 3, Normal: 2, Cache: 1}
	a := admission.New(pool, factor)

	cidStr := "bafkreiabcdef1234567890important"
	seedBlob(t, ctx, pool, cidStr, 1000)

	// Seed 5 eligible nodes with distinct reputations 0.1..0.5.
	nodeIDs := make([]uuid.UUID, 5)
	reps := []float32{0.1, 0.2, 0.3, 0.4, 0.5}
	for i := range nodeIDs {
		nodeIDs[i] = uuid.New()
		free := int64(100000)
		addr := "10.0.0." + string(rune('1'+i)) + ":5000"
		seedNode(t, ctx, pool, nodeIDs[i], reps[i], &free,
			[]string{"read-source/v1", "blob-transfer/v1"},
			"active", "probationary", addr)
	}

	assigned, err := a.Assign(ctx, cidStr, "important")
	require.NoError(t, err)
	require.Equal(t, 3, assigned) // R=3 for important

	count := queryPinAssignmentCount(t, ctx, pool, cidStr)
	require.Equal(t, 3, count, "exactly R=3 pin_assignments created")

	// Verify the 3 highest-reputation nodes (indices 2,3,4 with rep=30,40,50) were selected.
	assignedIDs := queryPinAssignmentNodeIDs(t, ctx, pool, cidStr)
	top3 := map[uuid.UUID]bool{nodeIDs[2]: true, nodeIDs[3]: true, nodeIDs[4]: true}
	for _, id := range assignedIDs {
		require.True(t, top3[id], "assigned node %s must be in top-3 reputation set", id)
	}
}

func TestAssignerSkipsNonSourceCapable(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedChangeLogState(t, ctx, pool)

	factor := config.ReplicationFactor{Important: 3, Normal: 2, Cache: 1}
	a := admission.New(pool, factor)

	cidStr := "bafkreiabcdef1234567890skiptest"
	seedBlob(t, ctx, pool, cidStr, 1000)

	free := int64(100000)
	// 1. Missing read-source/v1 capability.
	id1 := uuid.New()
	seedNode(t, ctx, pool, id1, 0.5, &free, []string{"blob-transfer/v1"}, "active", "probationary", "10.0.1.1:5000")

	// 2. Missing source_nebula_addr.
	id2 := uuid.New()
	seedNode(t, ctx, pool, id2, 0.5, &free, []string{"read-source/v1"}, "active", "probationary", "")

	// 3. Suspended (trust_state = 'suspended').
	id3 := uuid.New()
	seedNode(t, ctx, pool, id3, 0.5, &free, []string{"read-source/v1"}, "active", "suspended", "10.0.1.3:5000")

	// 4. Non-live status (evicted).
	id4 := uuid.New()
	seedNode(t, ctx, pool, id4, 0.5, &free, []string{"read-source/v1"}, "evicted", "probationary", "10.0.1.4:5000")

	// 5. Revoked status.
	id5 := uuid.New()
	seedNode(t, ctx, pool, id5, 0.5, &free, []string{"read-source/v1"}, "revoked", "probationary", "10.0.1.5:5000")

	assigned, err := a.Assign(ctx, cidStr, "important")
	require.NoError(t, err)
	require.Equal(t, 0, assigned, "no eligible nodes, so assigned=0")

	count := queryPinAssignmentCount(t, ctx, pool, cidStr)
	require.Equal(t, 0, count, "no pin_assignments created for ineligible nodes")

	_ = id1
	_ = id2
	_ = id3
	_ = id4
	_ = id5
}

func TestAssignerUnderReplicatedLogsAndPartial(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedChangeLogState(t, ctx, pool)

	factor := config.ReplicationFactor{Important: 3, Normal: 2, Cache: 1}
	a := admission.New(pool, factor)

	cidStr := "bafkreiabcdef1234567890partial"
	seedBlob(t, ctx, pool, cidStr, 1000)

	// Seed only 2 eligible nodes (< R=3 for important).
	free := int64(100000)
	id1 := uuid.New()
	seedNode(t, ctx, pool, id1, 0.8, &free, []string{"read-source/v1"}, "active", "probationary", "10.0.2.1:5000")
	id2 := uuid.New()
	seedNode(t, ctx, pool, id2, 0.6, &free, []string{"read-source/v1"}, "active", "probationary", "10.0.2.2:5000")

	assigned, err := a.Assign(ctx, cidStr, "important")
	require.NoError(t, err) // under-replicated is NOT an error — returns partial count
	require.Equal(t, 2, assigned, "partial: 2 out of 3")

	count := queryPinAssignmentCount(t, ctx, pool, cidStr)
	require.Equal(t, 2, count, "2 pin_assignments created (partial)")

	_ = id1
	_ = id2
}
