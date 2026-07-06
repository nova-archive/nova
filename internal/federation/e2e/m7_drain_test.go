// P2-M7 drain e2e capstone (D-M7-6): voluntary decommission with ZERO
// durability loss, over the REAL donor source transport. A draining sole
// holder is repaired FROM (repair source of last resort) into a fresh
// destination via a genuine loopback-mTLS fetch, the drain debt clears, and
// only then is the node revoked — healthy count unchanged end-to-end.
package e2e

import (
	"context"
	"crypto/ed25519"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/replay"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/bandwidth"
	"github.com/nova-archive/nova/internal/node/source"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/nova-archive/nova/internal/orchestrator"
	"github.com/stretchr/testify/require"
)

func m7pg(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	require.NoError(t, u.Scan(id))
	return u
}

// seedM7Node inserts a live, sync-current node; sourceable=true grants the
// full read/repair capability set + the (real) source address.
func seedM7Node(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, addr string, sourceable bool) {
	t.Helper()
	caps, src := "{pin-change-log/v1,snapshot/v1}", ""
	if sourceable {
		caps = "{pin-change-log/v1,snapshot/v1,read-source/v1,repair-stream/v1,audit-block-hash/v1}"
		src = addr
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint, capacity_bytes,
		                   bandwidth_budget_bytes_per_day, policy_filters, status, assignment_sync_state,
		                   trust_state, advertised_capabilities, source_nebula_addr, last_seen_at,
		                   last_free_bytes, last_egress_remaining_bytes)
		VALUES ($1::uuid, $2, $3, 1073741824, 1073741824, '{}', 'active', 'current',
		        'trusted', $4::text[], NULLIF($5,''), now(), 1000000000, 1000000000)`,
		id.String(), id.String()+"-nfp", id.String()+"-ffp", caps, src)
	require.NoError(t, err)
}

func TestE2EDrainDecommission(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	// target 1: draining the sole holder forces the repair 1→2 copies, and the
	// debt clears once ONE non-draining holder acks (the exact D-M7-6f gate).
	targets := orchestrator.ReplicationTargets{Important: 5, Normal: 1, Cache: 1}

	const cid = "bafyM7DRAINe2e"
	env := make([]byte, 4096)
	for i := range env {
		env[i] = byte('D')
	}

	// Blob + manifest (healCID reads envelope_size); NO local coordinator copy —
	// the repair MUST source from the draining donor.
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ($1, 'image/jpeg', 4096, 'active', 'image', 2)`, cid)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		VALUES ($1, 'sha2-256', 'raw', 'size-262144', 4096, 4096, 1)`, cid)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_storage_state (cid, commit_state, durability_class, local_role, local_present, local_bytes)
		VALUES ($1, 'committed', 'normal', 'absent', false, 0)`, cid)
	require.NoError(t, err)

	// REAL loopback-mTLS source donor A holding the envelope.
	caPEM, caKeyPEM, err := ca.GenerateCA()
	require.NoError(t, err)
	signer, err := tokens.NewSignerFromSeed(make([]byte, 32))
	require.NoError(t, err)
	pub, err := wire.DecodePublicKey(signer.PublicKeyWire())
	require.NoError(t, err)

	// Seed A's node row + acked pin first (the source server's progress record
	// must name the REAL assignment).
	aID, bID := uuid.New(), uuid.New()
	var aAssign uuid.UUID
	var aGen int64
	seedM7Node(t, ctx, pool, aID, "placeholder", true)
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pin_assignments (cid, node_id, state, acked_at)
		VALUES ($1, $2::uuid, 'acked', now())
		RETURNING assignment_id, generation`, cid, aID).Scan(&aAssign, &aGen))

	srcD := startSourceDonorWithID(t, aID, caPEM, caKeyPEM, pub, cid, env, aAssign, aGen, int64(len(env)), 1<<20)
	defer srcD.close()
	_, err = pool.Exec(ctx, `UPDATE nodes SET source_nebula_addr=$2 WHERE id=$1::uuid`, aID, srcD.addr)
	require.NoError(t, err)

	seedM7Node(t, ctx, pool, bID, "", false)

	// Build the projection row (target_count feeds the drain-debt query).
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, orchestrator.RecomputeCID(ctx, tx, cid, targets))
	require.NoError(t, tx.Commit(ctx))

	// 1. Drain A — the same SQL sequence the CLI core runs.
	aPg := m7pg(t, aID.String())
	n, err := q.SetNodeDraining(ctx, aPg)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = q.FailNodePendingAssignments(ctx, aPg)
	require.NoError(t, err)
	require.NoError(t, q.MarkReplicationDirtyForNode(ctx, aPg))
	require.NoError(t, q.EnqueueReconcileForNode(ctx, gen.EnqueueReconcileForNodeParams{Reason: "node_draining", NodeID: aPg}))

	debt, err := q.CountDrainPendingCIDs(ctx, gen.CountDrainPendingCIDsParams{NodeID: aPg, BelowFloorGraceSecs: config.DefaultBelowFloorGraceSeconds})
	require.NoError(t, err)
	require.EqualValues(t, 1, debt)

	// 2. Orchestrator tick: the repair is sourced FROM draining A into B.
	sch := orchestrator.NewScheduler(pool, orchestrator.SchedulerConfig{Targets: targets, ReputationFloor: 0.5})
	healed, err := sch.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, healed)

	var bAssign uuid.UUID
	var bGen int64
	var srcNode string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT assignment_id, generation, source_node_id::text
		FROM pin_assignments WHERE cid=$1 AND node_id=$2::uuid AND state='pending'`, cid, bID).
		Scan(&bAssign, &bGen, &srcNode))
	require.Equal(t, aID.String(), srcNode, "repair token source == draining A (D-M7-6c)")

	// 3. REAL transport: B fetches the envelope from A over loopback mTLS with
	// a grant bound to A's acked assignment and B's pending one.
	tok := mintRepairGrant(t, signer, aID, bID, cid, aAssign, aGen, bAssign, bGen, int64(len(env)))
	resp, body := fetchRepair(t, caPEM, caKeyPEM, bID, srcD.addr, cid, tok)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, env, body, "the draining source streams exactly the ciphertext envelope")

	// B acks → drain debt clears.
	acked, err := q.AckPinAssignment(ctx, gen.AckPinAssignmentParams{
		Cid: cid, NodeID: m7pg(t, bID.String()),
		AssignmentID: m7pg(t, bAssign.String()), Generation: bGen,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, acked)
	debt, err = q.CountDrainPendingCIDs(ctx, gen.CountDrainPendingCIDsParams{NodeID: aPg, BelowFloorGraceSecs: config.DefaultBelowFloorGraceSeconds})
	require.NoError(t, err)
	require.EqualValues(t, 0, debt, "drain-ready (safe-to-revoke gate)")

	// 4. Revoke A → ZERO durability loss: B carries the CID alone.
	_, err = q.RevokeNode(ctx, aPg)
	require.NoError(t, err)
	counts, err := q.RecomputeReplicationCounts(ctx, gen.RecomputeReplicationCountsParams{
		Cid: cid, BelowFloorGraceSecs: orchestrator.DefaultBelowFloorGraceSeconds,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, counts.HealthyAcked, "healthy count unchanged end-to-end")
}

// TestE2EDrainDebtDistrustsBelowFloorHolder (P2-M7.1, D-M7.1-3): the drain
// safe-to-revoke gate must not lean on a distrusted replica. When the ONLY
// other holder of a draining node's CID is SUSTAINED-below-floor, drain debt
// stays non-zero (revoking now would leave the data solely on a node healing
// is actively replacing); an in-grace marker still counts (hysteresis).
// Mirrors: a pending replacement ON a sustained-below-floor destination must
// not read as drain progress either (CountDrainInflightCIDs).
func TestE2EDrainDebtDistrustsBelowFloorHolder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	targets := orchestrator.ReplicationTargets{Important: 5, Normal: 1, Cache: 1}
	const grace = 3600.0

	const cid = "bafyM71DRAINbf"
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ($1, 'image/jpeg', 4096, 'active', 'image', 2)`, cid)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_storage_state (cid, commit_state, durability_class, local_role, local_present, local_bytes)
		VALUES ($1, 'committed', 'normal', 'absent', false, 0)`, cid)
	require.NoError(t, err)

	// Draining node D and below-floor node B both hold the CID acked.
	dID, bID := uuid.New(), uuid.New()
	seedM7Node(t, ctx, pool, dID, "placeholder", true)
	seedM7Node(t, ctx, pool, bID, "placeholder", true)
	for _, n := range []uuid.UUID{dID, bID} {
		_, err = pool.Exec(ctx, `
			INSERT INTO pin_assignments (cid, node_id, state, acked_at)
			VALUES ($1, $2::uuid, 'acked', now())`, cid, n)
		require.NoError(t, err)
	}
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, orchestrator.RecomputeCID(ctx, tx, cid, targets))
	require.NoError(t, tx.Commit(ctx))

	dPg := m7pg(t, dID.String())
	n, err := q.SetNodeDraining(ctx, dPg)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	// Healthy other holder B → the debt is already covered.
	debt, err := q.CountDrainPendingCIDs(ctx, gen.CountDrainPendingCIDsParams{
		NodeID: dPg, BelowFloorGraceSecs: grace,
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, debt, "a healthy other holder covers target 1")

	// B sinks below the floor, SUSTAINED (2h-old marker, 1h grace): the gate
	// must reopen — a distrusted replica cannot make a drain safe-to-revoke.
	_, err = pool.Exec(ctx, `
		UPDATE nodes SET below_floor_since = now() - interval '2 hours' WHERE id = $1::uuid`, bID)
	require.NoError(t, err)
	debt, err = q.CountDrainPendingCIDs(ctx, gen.CountDrainPendingCIDsParams{
		NodeID: dPg, BelowFloorGraceSecs: grace,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, debt, "drain debt stays non-zero when the only other holder is sustained-below-floor (D-M7.1-3)")

	// In-grace marker (1 min old): hysteresis — B still counts, debt covered.
	_, err = pool.Exec(ctx, `
		UPDATE nodes SET below_floor_since = now() - interval '1 minute' WHERE id = $1::uuid`, bID)
	require.NoError(t, err)
	debt, err = q.CountDrainPendingCIDs(ctx, gen.CountDrainPendingCIDsParams{
		NodeID: dPg, BelowFloorGraceSecs: grace,
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, debt, "an in-grace below-floor holder still counts (hysteresis)")

	// In-flight mirror: a pending replacement on a SUSTAINED-below-floor
	// destination is not progress; on a healthy destination it is.
	cID := uuid.New()
	seedM7Node(t, ctx, pool, cID, "placeholder", true)
	_, err = pool.Exec(ctx, `
		UPDATE nodes SET below_floor_since = now() - interval '2 hours' WHERE id = ANY(ARRAY[$1,$2]::uuid[])`, bID, cID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO pin_assignments (cid, node_id, state) VALUES ($1, $2::uuid, 'pending')`, cid, cID)
	require.NoError(t, err)
	inflight, err := q.CountDrainInflightCIDs(ctx, gen.CountDrainInflightCIDsParams{
		NodeID: dPg, BelowFloorGraceSecs: grace,
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, inflight, "a pending on a sustained-below-floor destination is not drain progress")
	_, err = pool.Exec(ctx, `UPDATE nodes SET below_floor_since = NULL WHERE id = $1::uuid`, cID)
	require.NoError(t, err)
	inflight, err = q.CountDrainInflightCIDs(ctx, gen.CountDrainInflightCIDsParams{
		NodeID: dPg, BelowFloorGraceSecs: grace,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, inflight, "a pending on a healthy destination is drain progress")
}

// startSourceDonorWithID is startSourceDonor with a caller-fixed node id (the
// drain capstone needs the DB row and the serving donor to be the SAME node —
// the grant's SourceNodeID binds to the server's own identity).
func startSourceDonorWithID(t *testing.T, nodeID uuid.UUID, caPEM, caKeyPEM []byte, pub ed25519.PublicKey, cid string, env []byte, srcAssignID uuid.UUID, srcGen, byteSize, dailyBudget int64) *srcDonor {
	t.Helper()
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{
		DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"},
	})
	require.NoError(t, err)
	tlsCfg, err := transport.ServerTLSConfig(caPEM, srvPEM, srvKeyPEM)
	require.NoError(t, err)

	budget := bandwidth.NewDailyBucket(dailyBudget, time.Now())
	handler := source.NewServer(source.Deps{
		Pinner:      &memPinner{data: map[string][]byte{cid: env}},
		Budget:      budget,
		PubKey:      staticPub{pub: pub},
		Progress:    oneProgress{cid: cid, p: state.Progress{AssignmentID: srcAssignID.String(), Generation: srcGen, ByteSize: byteSize, State: state.ProgressAckDelivered}},
		NodeID:      nodeID.String(),
		BootTime:    time.Now().Add(-time.Minute),
		ReplayCache: replay.New(),
		Now:         time.Now,
	})
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ln := transport.NewTLSListener(inner, tlsCfg)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return &srcDonor{nodeID: nodeID, addr: inner.Addr().String(), budget: budget, ln: ln, srv: srv}
}
