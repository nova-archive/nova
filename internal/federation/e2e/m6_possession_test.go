// This file is the P2-M6 integration capstone: it proves the possession-audit
// round-trip end-to-end over a genuine mTLS handshake against the REAL donor
// source server (coordinator-role gate + assignment binding + local-only block
// read), driving the REAL possession Dispatcher and Auditor against a real
// Postgres (testcontainers) so the outcome TRANSACTION (pin_audits row,
// reputation move, pin-assignment fail, reconcile enqueue, trust state machine)
// is exercised for real.
//
// Approach (binding): drive the Dispatcher + Auditor DIRECTLY over loopback
// mTLS rather than the Scheduler's ticker. The scheduler's two-stage sampling is
// already DB-unit-tested (Task 9); the E2E's job is the mTLS audit round-trip +
// CID-reconstruction verify + outcome transaction, which is deterministic this
// way (no sampling randomness) and needs no exported RunOnce.
//
// Authored for P2-M6 by Bug.
package e2e

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/audit/possession"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/replay"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/bandwidth"
	"github.com/nova-archive/nova/internal/node/source"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/nova-archive/nova/internal/notify"
)

// --- donor-side audit fakes ---------------------------------------------------

// errBlockNotLocal stands in for ipfsclient.ErrBlockNotLocal — the handler treats
// ANY BlockGetLocal error as a clean 404, so a local sentinel keeps the e2e off
// the node ipfsclient package without changing behaviour.
var errBlockNotLocal = errors.New("audit: block not present locally (fake reader)")

// fakeAuditReader is a concurrency-safe in-memory AuditBlockReader. It serves
// blocks it holds and returns errBlockNotLocal once a block is dropped — it never
// fetches from a peer (mirroring Kubo's offline=true local-only read).
type fakeAuditReader struct {
	mu     sync.Mutex
	blocks map[string][]byte
}

func (r *fakeAuditReader) BlockGetLocal(_ context.Context, blockCID string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.blocks[blockCID]
	if !ok {
		return nil, errBlockNotLocal
	}
	return b, nil
}

func (r *fakeAuditReader) drop(blockCID string) {
	r.mu.Lock()
	delete(r.blocks, blockCID)
	r.mu.Unlock()
}

// capturingNotifier records every emitted event so a test can assert on the
// presence/absence of a signal (e.g. federation.node_suspect).
type capturingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *capturingNotifier) Emit(_ context.Context, ev notify.Event) {
	n.mu.Lock()
	n.events = append(n.events, ev)
	n.mu.Unlock()
}

func (n *capturingNotifier) count(typ string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, e := range n.events {
		if e.Type == typ {
			c++
		}
	}
	return c
}

// auditDonor is a running real-mTLS donor source server with the M6 audit deps
// wired (POST /fed/v1/audit/challenge).
type auditDonor struct {
	nodeID uuid.UUID
	addr   string // full https URL, e.g. https://127.0.0.1:NNNNN
	reader *fakeAuditReader
	close  func()
}

// startAuditDonor stands up the REAL source server over loopback mTLS, holding
// `envelope` for blobCID with an acked progress record (assignID/generation) and
// `blockBytes` for blockCID in its local audit reader.
func startAuditDonor(t *testing.T, caPEM, caKeyPEM []byte, pub ed25519.PublicKey,
	nodeID, assignID uuid.UUID, generation int64,
	blobCID string, envelope []byte, blockCID string, blockBytes []byte,
) *auditDonor {
	t.Helper()
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{
		DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"},
	})
	require.NoError(t, err)
	tlsCfg, err := transport.ServerTLSConfig(caPEM, srvPEM, srvKeyPEM)
	require.NoError(t, err)

	reader := &fakeAuditReader{blocks: map[string][]byte{blockCID: blockBytes}}
	handler := source.NewServer(source.Deps{
		Pinner:   &memPinner{data: map[string][]byte{blobCID: envelope}},
		Budget:   bandwidth.NewDailyBucket(1<<30, time.Now()),
		PubKey:   staticPub{pub: pub},
		Progress: oneProgress{cid: blobCID, p: state.Progress{AssignmentID: assignID.String(), Generation: generation, ByteSize: int64(len(envelope)), State: state.ProgressAckDelivered}},
		NodeID:   nodeID.String(),
		BootTime: time.Now().Add(-time.Minute),
		// Audit deps.
		AuditBlocks: reader,
		AuditBudget: bandwidth.NewDailyBucket(1<<30, time.Now()),
		// (MaxAuditBlockBytes 0 -> server defaults to 262144.)
		ReplayCache: replay.New(),
		Now:         time.Now,
	})
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ln := transport.NewTLSListener(inner, tlsCfg)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return &auditDonor{
		nodeID: nodeID,
		addr:   "https://" + inner.Addr().String(),
		reader: reader,
		close:  func() { _ = srv.Close(); _ = ln.Close() },
	}
}

// --- DB seeding ---------------------------------------------------------------

// seedAuditFixture seeds the FK chain the outcome transaction touches: a blob +
// its manifest + a single audited block, an audit-capable donor node whose
// source address points at the running donor, and an acked pin assignment with
// an explicit assignment_id/generation matching the donor's progress record.
func seedAuditFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	nodeID, assignID uuid.UUID, generation int64,
	donorAddr, blobCID, blockCID string, blockSize, envelopeSize int64,
) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO blobs (cid, mime_type, byte_size) VALUES ($1, 'application/octet-stream', $2)`,
		blobCID, envelopeSize)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO blob_manifests (cid, cid_version, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		 VALUES ($1, 1, 'sha2-256', 'raw', 'size-262144', $2, $2, 1)`,
		blobCID, envelopeSize)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO blob_blocks (blob_cid, block_cid, block_index, block_size) VALUES ($1, $2, 0, $3)`,
		blobCID, blockCID, blockSize)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint, capacity_bytes,
		                   bandwidth_budget_bytes_per_day, policy_filters, status,
		                   assignment_sync_state, advertised_capabilities, source_nebula_addr,
		                   reputation_score, trust_state)
		VALUES ($1, 'fp-m6', 'ffp-m6', 1099511627776, 1099511627776, '{}', 'active',
		        'current', ARRAY['audit-block-hash/v1'], $2, 0.9, 'probationary')
	`, nodeID.String(), donorAddr)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO pin_assignments (cid, node_id, state, assignment_id, generation, acked_at)
		VALUES ($1, $2, 'acked', $3, $4, now())
	`, blobCID, nodeID.String(), assignID.String(), generation)
	require.NoError(t, err)
}

// --- one full audit cycle over real mTLS --------------------------------------

// runAudit performs one real audit: insert-before-dispatch, challenge the donor
// over mTLS, then record the outcome in its transaction. Returns the audit id and
// the dispatch result.
func runAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	dispatcher *possession.Dispatcher, auditor *possession.Auditor, donor *auditDonor,
	assignID uuid.UUID, generation int64, blobCID, blockCID string, blockSize int64,
) (uuid.UUID, possession.DispatchResult) {
	t.Helper()
	q := gen.New(pool)
	auditID := uuid.New()
	nonce := uuid.NewString()
	deadline := time.Now().Add(30 * time.Second)

	require.NoError(t, q.InsertAuditChallenge(ctx, gen.InsertAuditChallengeParams{
		ID:            pgUUID(auditID),
		BlobCid:       blobCID,
		NodeID:        pgUUID(donor.nodeID),
		ChallengeKind: wire.AuditChallengeKindBlockHash,
		Nonce:         nonce,
		Deadline:      deadline,
	}))

	res, err := dispatcher.Challenge(ctx, donor.addr, wire.AuditChallenge{
		ChallengeID:  auditID.String(),
		BlobCID:      blobCID,
		AssignmentID: assignID.String(),
		Generation:   generation,
		BlockIndex:   0,
		BlockCID:     blockCID,
		BlockSize:    blockSize,
		Nonce:        nonce,
	})
	require.NoError(t, err)

	require.NoError(t, auditor.Record(ctx, possession.AuditTarget{
		AuditID:      auditID.String(),
		NodeID:       donor.nodeID.String(),
		BlobCID:      blobCID,
		BlockCID:     blockCID,
		AssignmentID: assignID.String(),
		Generation:   generation,
		Nonce:        nonce,
		BlockIndex:   0,
		BlockSize:    blockSize,
		Deadline:     deadline,
	}, res, 0.5 /* reputation floor */))
	return auditID, res
}

// coordinatorDispatcher builds the real possession Dispatcher with a
// COORDINATOR-role client identity (the audit endpoint 403s RoleNode).
func coordinatorDispatcher(t *testing.T, caPEM, caKeyPEM []byte) *possession.Dispatcher {
	t.Helper()
	cliPEM, cliKeyPEM, err := ca.IssueCoordinatorClientCert(caPEM, caKeyPEM, uuid.New())
	require.NoError(t, err)
	cliTLS, err := transport.ClientTLSConfig(caPEM, cliPEM, cliKeyPEM)
	require.NoError(t, err)
	cliTLS.ServerName = "localhost"
	return possession.NewDispatcher(cliTLS)
}

// --- tests --------------------------------------------------------------------

func TestE2EPossessionPassThenLyingDonor(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	caPEM, caKeyPEM, err := ca.GenerateCA()
	require.NoError(t, err)
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	blockBytes := bytesRepeat('B', 4096)
	envelope := bytesRepeat('E', 8192)
	blockCID := rawCID(t, blockBytes)
	blobCID := rawCID(t, envelope) // a distinct real CID for the envelope
	blockSize := int64(len(blockBytes))

	nodeID, assignID := uuid.New(), uuid.New()
	const generation = int64(1)

	// Donor first (loopback :0), so we can seed source_nebula_addr = its addr.
	donor := startAuditDonor(t, caPEM, caKeyPEM, pub, nodeID, assignID, generation, blobCID, envelope, blockCID, blockBytes)
	defer donor.close()
	seedAuditFixture(t, ctx, pool, nodeID, assignID, generation, donor.addr, blobCID, blockCID, blockSize, int64(len(envelope)))

	notifier := &capturingNotifier{}
	auditor := possession.NewAuditor(pool, notifier, possession.DefaultTrustConfig())
	dispatcher := coordinatorDispatcher(t, caPEM, caKeyPEM)

	// 1) Honest donor: the challenge passes over real mTLS, the outcome row records
	// a pass, the pin stays acked, and reputation drifts up.
	auditID1, res1 := runAudit(t, ctx, pool, dispatcher, auditor, donor, assignID, generation, blobCID, blockCID, blockSize)
	require.Equal(t, possession.OutcomePass, res1.Outcome)
	require.Equal(t, "pass", auditResult(t, ctx, pool, auditID1))
	require.Equal(t, "acked", pinState(t, ctx, pool, blobCID, nodeID))
	repAfter1 := reputation(t, ctx, pool, nodeID)
	require.Greater(t, repAfter1, 0.9, "an honest pass drifts reputation up")

	// 2) No coordinator origin (D-M6-15 #1): the Dispatcher verifies the outcome
	// PURELY from the donor's returned block bytes (CID reconstruction) — there is
	// NO coordinator-local block source anywhere in this path, so the absence of a
	// local origin copy cannot affect the verdict. A second fresh challenge still
	// passes.
	auditID2, res2 := runAudit(t, ctx, pool, dispatcher, auditor, donor, assignID, generation, blobCID, blockCID, blockSize)
	require.Equal(t, possession.OutcomePass, res2.Outcome)
	require.Equal(t, "pass", auditResult(t, ctx, pool, auditID2))

	// 3) Lying donor: drop the block locally so the donor 404s. The outcome is a
	// hard not-present fail: the audit records 'fail', the acked pin is failed, a
	// reconcile row is enqueued, and reputation is multiplied down below the floor.
	donor.reader.drop(blockCID)
	auditID3, res3 := runAudit(t, ctx, pool, dispatcher, auditor, donor, assignID, generation, blobCID, blockCID, blockSize)
	require.Equal(t, possession.OutcomeFailNotPresent, res3.Outcome)
	require.Equal(t, "fail", auditResult(t, ctx, pool, auditID3))
	require.Equal(t, "failed", pinState(t, ctx, pool, blobCID, nodeID))
	require.Equal(t, "audit_not_present", reconcileReason(t, ctx, pool, blobCID))
	require.Less(t, reputation(t, ctx, pool, nodeID), 0.6, "a not-present fail multiplies reputation down")

	// not_present is NOT a hash mismatch, so it raises no node_suspect signal.
	require.Equal(t, 0, notifier.count("federation.node_suspect"))
}

// TestE2EBlockGetLocalNoNetworkFetch (D-M6-15 #12): a donor that does not hold the
// block LOCALLY returns 404 — it does not pass by fetching the block from a peer.
//
// Limitation: this fake AuditBlockReader proves the 404 -> no-pass contract (the
// handler maps any BlockGetLocal error to 404). The actual Bitswap suppression —
// Kubo's `offline=true` block/get making a local-only read — is a runtime property
// of the real ipfsclient, covered by the Task 2 unit test and the offline=true
// query param; it is not reproducible against a fake Kubo here.
func TestE2EBlockGetLocalNoNetworkFetch(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	caPEM, caKeyPEM, err := ca.GenerateCA()
	require.NoError(t, err)
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	blockBytes := bytesRepeat('B', 4096)
	envelope := bytesRepeat('E', 8192)
	blockCID := rawCID(t, blockBytes)
	blobCID := rawCID(t, envelope)
	blockSize := int64(len(blockBytes))

	nodeID, assignID := uuid.New(), uuid.New()
	const generation = int64(1)

	donor := startAuditDonor(t, caPEM, caKeyPEM, pub, nodeID, assignID, generation, blobCID, envelope, blockCID, blockBytes)
	defer donor.close()
	// The donor is pinned + acked for the blob but does NOT hold the block locally:
	// drop it before any challenge. A peer "having" it is irrelevant — the donor
	// reads local-only.
	donor.reader.drop(blockCID)
	seedAuditFixture(t, ctx, pool, nodeID, assignID, generation, donor.addr, blobCID, blockCID, blockSize, int64(len(envelope)))

	auditor := possession.NewAuditor(pool, &capturingNotifier{}, possession.DefaultTrustConfig())
	dispatcher := coordinatorDispatcher(t, caPEM, caKeyPEM)

	auditID, res := runAudit(t, ctx, pool, dispatcher, auditor, donor, assignID, generation, blobCID, blockCID, blockSize)
	require.Equal(t, possession.OutcomeFailNotPresent, res.Outcome, "no local block -> 404, no network fetch")
	require.Equal(t, "fail", auditResult(t, ctx, pool, auditID))
}

// --- small query/helpers ------------------------------------------------------

func auditResult(t *testing.T, ctx context.Context, pool *pgxpool.Pool, auditID uuid.UUID) string {
	t.Helper()
	var result string
	require.NoError(t, pool.QueryRow(ctx, `SELECT result::text FROM pin_audits WHERE id = $1`, auditID.String()).Scan(&result))
	return result
}

func pinState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string, nodeID uuid.UUID) string {
	t.Helper()
	var st string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state::text FROM pin_assignments WHERE cid = $1 AND node_id = $2`, cidStr, nodeID.String()).Scan(&st))
	return st
}

func reputation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID uuid.UUID) float64 {
	t.Helper()
	var rep float64
	require.NoError(t, pool.QueryRow(ctx, `SELECT reputation_score FROM nodes WHERE id = $1`, nodeID.String()).Scan(&rep))
	return rep
}

func reconcileReason(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) string {
	t.Helper()
	var reason string
	require.NoError(t, pool.QueryRow(ctx, `SELECT reason FROM blob_replication_reconcile_queue WHERE cid = $1`, cidStr).Scan(&reason))
	return reason
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// rawCID builds a CIDv1 raw/sha2-256 CID for the bytes (the dispatcher's verify
// idiom: reconstruct from the stored prefix and compare).
func rawCID(t *testing.T, b []byte) string {
	t.Helper()
	c, err := cid.V1Builder{Codec: cid.Raw, MhType: mh.SHA2_256}.Sum(b)
	require.NoError(t, err)
	return c.String()
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
