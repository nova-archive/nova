package coordinator_test

// P2-M4.1 end-to-end composition test for the storage/read redirect.
//
// PLACEMENT NOTE (load-bearing): this test lives in pkg/coordinator (external
// test package coordinator_test), NOT in internal/federation/coordinator. The
// latter is imported by pkg/coordinator/admission and pkg/coordinator/storage,
// so a test there importing admission/storage would be an import cycle.
// pkg/coordinator is top-level (imported by nothing), so it may freely import
// storage, admission, internal/node/source, internal/federation/{ca,tokens,
// transport,wire,replay}, internal/node/bandwidth, internal/dbtest, and
// internal/db/gen.
//
// What this proves end-to-end with REAL wire components:
//
//   - Step 1: a gate-on Put writes a 'staging' blob — NOT publicly visible
//     (Resolve ⇒ ErrStagingNotVisible) and OnCommitted does NOT fire.
//   - Step 2: once 2 acked sourceable holders exist, one ReconcileOnce pass
//     commits the blob (staging → committed), fires OnCommitted exactly once,
//     and the blob becomes visible.
//   - Step 3: one PruneOnce pass (sourceable=2 >= floor=2) unpins the origin
//     copy from the coordinator backend.
//   - Step 4: a cold OpenBytes fetches the ciphertext envelope from a REAL donor
//     read-source server over loopback mTLS, verifies-before-serve (deterministic
//     re-import → CID match) and returns the correct bytes; a second OpenBytes is
//     a LOCAL cache hit (the donor's served-request counter does not advance).
//   - Step 5: with both donor servers blackholed and their nodes made
//     non-sourceable, OpenBytes returns the 503 sentinel ErrNoSourceableHolder
//     (NOT ErrBlobNotFound).
//
// The novel M4.1 donor read path is exercised with a genuine mTLS handshake and
// the donor's full verify chain (role + signed read-grant + binding + local
// progress state + boot-floor + single-use replay + pin + egress debit). The
// blob is stored unencrypted (public_archival) so OpenBytes streams plaintext
// directly without a keystore decrypt — but the coordinator's verify-before-serve
// gate (AddDeterministic CID re-check) is identical to the encrypted path and is
// fully exercised here.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	gocid "github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/replay"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/node/bandwidth"
	"github.com/nova-archive/nova/internal/node/source"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/nova-archive/nova/pkg/coordinator/admission"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// ---------------------------------------------------------------------------
// In-memory coordinator origin backend (full ipfs.Backend). AddDeterministic
// computes the canonical CIDv1(raw, sha2-256) so verify-before-serve is genuine.
// Mirrors echoBackend in pkg/coordinator/storage/readsource_test.go (that one is
// package-internal; this external test re-implements the same shape).
// ---------------------------------------------------------------------------

type memBackend struct {
	mu    sync.Mutex
	store map[string][]byte
}

func newMemBackend() *memBackend { return &memBackend{store: map[string][]byte{}} }

func (b *memBackend) AddDeterministic(_ context.Context, env []byte) (ipfs.AddResult, error) {
	mh, err := multihash.Sum(env, multihash.SHA2_256, -1)
	if err != nil {
		return ipfs.AddResult{}, err
	}
	c := gocid.NewCidV1(gocid.Raw, mh)
	b.mu.Lock()
	b.store[c.String()] = append([]byte(nil), env...)
	b.mu.Unlock()
	return ipfs.AddResult{
		CID: c, MerkleRoot: c, EnvelopeSize: int64(len(env)), Codec: "raw",
		Blocks: []ipfs.Block{{CID: c, Index: 0, Size: len(env)}},
	}, nil
}

func (b *memBackend) Get(_ context.Context, c gocid.Cid) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.store[c.String()]
	if !ok {
		return nil, errors.New("memBackend: not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *memBackend) Has(_ context.Context, c gocid.Cid) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.store[c.String()]
	return ok, nil
}

func (b *memBackend) Pin(context.Context, gocid.Cid) error { return nil }

func (b *memBackend) Unpin(_ context.Context, c gocid.Cid) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.store, c.String())
	return nil
}

func (b *memBackend) BlockstoreHas(context.Context, gocid.Cid) (bool, error) { return false, nil }
func (b *memBackend) BlockGet(context.Context, gocid.Cid) ([]byte, error) {
	return nil, errors.New("unused")
}
func (b *memBackend) Close(context.Context) error  { return nil }
func (b *memBackend) Health(context.Context) error { return nil }

// ---------------------------------------------------------------------------
// Donor read-source dependency stubs.
// ---------------------------------------------------------------------------

// donorPinner is the donor's local pin store: a content-addressed map plus a
// served-request counter so the cache-hit assertion (step 4) can observe that a
// second coordinator read does NOT hit the donor again.
type donorPinner struct {
	mu     sync.Mutex
	store  map[string][]byte
	served atomic.Int64
}

func newDonorPinner() *donorPinner { return &donorPinner{store: map[string][]byte{}} }

func (p *donorPinner) put(cid string, data []byte) {
	p.mu.Lock()
	p.store[cid] = append([]byte(nil), data...)
	p.mu.Unlock()
}

func (p *donorPinner) Has(_ context.Context, cid string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.store[cid]
	return ok, nil
}

func (p *donorPinner) Get(_ context.Context, cid string) (io.ReadCloser, error) {
	p.mu.Lock()
	data, ok := p.store[cid]
	p.mu.Unlock()
	if !ok {
		return nil, errors.New("donorPinner: not found")
	}
	p.served.Add(1)
	return io.NopCloser(bytes.NewReader(data)), nil
}

// staticPubKey is a fail-open PubKeyProvider returning a fixed coordinator
// repair-token public key (matching the coordinator's signer).
type staticPubKey struct{ pub ed25519.PublicKey }

func (k staticPubKey) Current() (ed25519.PublicKey, bool) { return k.pub, true }

// staticProgress is the donor's local fetch/verify/ack record: it returns one
// acked-delivered entry for the test cid whose assignment_id/generation/byteSize
// match the seeded pin_assignments row (so the donor's binding + size checks pass).
type staticProgress struct {
	cid string
	p   state.Progress
}

func (s staticProgress) Get(cid string) (state.Progress, bool) {
	if cid == s.cid {
		return s.p, true
	}
	return state.Progress{}, false
}

// donorEnv bundles a running donor read-source server with the knobs the test
// drives: its listen addr (advertised as source_nebula_addr), its node_id, its
// pin store (served counter), and a way to blackhole it.
type donorEnv struct {
	nodeID   uuid.UUID
	addr     string
	pinner   *donorPinner
	listener net.Listener
	srv      *http.Server
}

func (d *donorEnv) close() {
	if d.srv != nil {
		_ = d.srv.Close()
	}
	if d.listener != nil {
		_ = d.listener.Close()
	}
}

// startDonor builds and starts a donor read-source server over loopback mTLS.
// The donor holds envBytes for cid in its pinner; its Progress reports the blob
// acked-delivered with the given assignment_id/generation/byteSize.
func startDonor(t *testing.T, caPEM []byte, pub ed25519.PublicKey, cid string, envBytes []byte, assignmentID uuid.UUID, generation, byteSize int64) *donorEnv {
	t.Helper()
	nodeID := uuid.New()

	serverCertPEM, serverKeyPEM, err := ca.IssueServerCert(caPEM, caKeyPEMForTest, ca.ServerCertOptions{
		DNSNames:    []string{"localhost"},
		IPAddresses: []string{"127.0.0.1"},
	})
	require.NoError(t, err)
	tlsCfg, err := transport.ServerTLSConfig(caPEM, serverCertPEM, serverKeyPEM)
	require.NoError(t, err)

	pinner := newDonorPinner()
	pinner.put(cid, envBytes)

	prog := staticProgress{
		cid: cid,
		p: state.Progress{
			AssignmentID: assignmentID.String(),
			Generation:   generation,
			ByteSize:     byteSize,
			State:        state.ProgressAckDelivered,
		},
	}

	handler := source.NewServer(source.Deps{
		Pinner:      pinner,
		Budget:      bandwidth.NewDailyBucket(1<<30, time.Now()),
		PubKey:      staticPubKey{pub: pub},
		Progress:    prog,
		NodeID:      nodeID.String(),
		BootTime:    time.Now().Add(-time.Minute), // boot floor in the past so fresh grants pass
		ReplayCache: replay.New(),
		Now:         time.Now,
	})

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ln := transport.NewTLSListener(inner, tlsCfg)

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	return &donorEnv{
		nodeID:   nodeID,
		addr:     inner.Addr().String(),
		pinner:   pinner,
		listener: ln,
		srv:      srv,
	}
}

// caKeyPEMForTest is package-level so startDonor can mint server certs without
// threading the CA key through every signature. Set once at the top of the test.
var caKeyPEMForTest []byte

// ---------------------------------------------------------------------------
// Recording product hook: counts OnCommitted invocations.
// ---------------------------------------------------------------------------

type recordingHook struct {
	mu        sync.Mutex
	committed []storage.CommittedRef
}

func (h *recordingHook) Analyze(context.Context, storage.PutContext, []byte) (storage.AnalyzeResult, error) {
	// ActionAllow so Put proceeds (a zero ScanResult would be rejected as
	// moderation-denied). No transform — store the original bytes.
	return storage.AnalyzeResult{Scan: storage.ScanResult{Action: storage.ActionAllow}}, nil
}
func (h *recordingHook) OnCommitted(_ context.Context, ref storage.CommittedRef) {
	h.mu.Lock()
	h.committed = append(h.committed, ref)
	h.mu.Unlock()
}
func (h *recordingHook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.committed)
}

// ---------------------------------------------------------------------------
// DB seeding helpers (this is package coordinator_test, so we seed via raw
// pool.Exec + gen.New, mirroring the idioms in pkg/coordinator/storage's own
// reconciler_test.go / prune_test.go).
// ---------------------------------------------------------------------------

// seedSourceableNode inserts a node satisfying the Count/ListSourceableHolders
// predicates: status=active, trust_state=trusted, read-source/v1 capability,
// source_nebula_addr = the donor's read-source listen addr, last_seen_at=now().
func seedSourceableNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID uuid.UUID, sourceAddr string) {
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
			$3, now()
		)
	`, pgtype.UUID{Bytes: nodeID, Valid: true}, idStr, sourceAddr)
	require.NoError(t, err)
}

// seedAckedHolder inserts a pin_assignments row in state='acked' with an explicit
// assignment_id + generation so the donor's Progress can be configured to match.
func seedAckedHolder(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string, nodeID, assignmentID uuid.UUID, generation int64) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO pin_assignments (cid, node_id, state, assignment_id, generation)
		VALUES ($1, $2, 'acked', $3, $4)
	`, cid,
		pgtype.UUID{Bytes: nodeID, Valid: true},
		pgtype.UUID{Bytes: assignmentID, Valid: true},
		generation)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// The composition test.
// ---------------------------------------------------------------------------

func TestM41ReadRedirectE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed e2e test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	// --- CA + coordinator client identity + signer/pubkey -------------------
	caCertPEM, caKeyPEM, err := ca.GenerateCA()
	require.NoError(t, err)
	caKeyPEMForTest = caKeyPEM

	coordID := uuid.New()
	coordCertPEM, coordKeyPEM, err := ca.IssueCoordinatorClientCert(caCertPEM, caKeyPEM, coordID)
	require.NoError(t, err)
	coordClientTLS, err := transport.CoordinatorClientTLS(caCertPEM, coordCertPEM, coordKeyPEM)
	require.NoError(t, err)

	seed := make([]byte, 32) // deterministic seed; signer + donor pubkey share it
	signer, err := tokens.NewSignerFromSeed(seed)
	require.NoError(t, err)
	donorPub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	// --- coordinator origin backend + keystore + public_archival collection -
	backend := newMemBackend()

	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	// public_archival collection ⇒ Put stores plaintext (deterministic CID),
	// OpenBytes streams it directly (no keystore decrypt needed).
	var ownerID, collectionID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		uuid.NewString()+"@m41.e2e").Scan(&ownerID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
		 VALUES ($1,'m41','m41','public',true) RETURNING id`, ownerID).Scan(&collectionID))

	// --- storage Service: bounded_cache + commit gate + assigner + donor tier
	quorum := config.ReplicationFactor{Important: 2, Normal: 2, Cache: 1}
	hook := &recordingHook{}
	svc := storage.NewService(pool, backend, ks,
		storage.WithProductHook(hook),
		storage.WithStorageMode(storage.StorageModeConfig{
			Mode: storage.StorageModeBoundedCache, MaxBytes: 4096, ProtectedRatio: 0.8,
		}),
		storage.WithCommitGate(storage.CommitGateConfig{
			RequireQuorum:      true,
			Quorum:             quorum,
			ReconcilerInterval: time.Hour, // we drive ReconcileOnce manually
			FailAfter:          time.Hour,
			StaleSeconds:       3600,
		}),
		storage.WithAssigner(admission.New(pool, quorum)),
		storage.WithDonorReadSource(coordClientTLS, signer, storage.ReadSourceConfig{
			TTL: time.Hour, StaleSecs: 3600,
		}),
	)
	reconciler := storage.NewReconciler(svc)
	require.NotNil(t, reconciler, "reconciler must be non-nil when the gate is on")
	pruner := storage.NewPruner(svc, storage.PrunerConfig{Floor: 2, StaleSeconds: 3600, Interval: time.Hour})
	require.NotNil(t, pruner, "pruner must be non-nil in bounded_cache mode")

	// =======================================================================
	// STEP 1: gate-on Put ⇒ staging, not visible, OnCommitted NOT fired.
	// =======================================================================
	plaintext := []byte("nova M4.1 read-redirect e2e: the canonical archival object")
	put, err := svc.Put(ctx, bytes.NewReader(plaintext), int64(len(plaintext)),
		storage.PutContext{MIME: "text/plain", Product: "raw", CollectionID: &collectionID})
	require.NoError(t, err)
	require.Equal(t, "staging", put.DurabilityState, "gate-on Put must report staging")
	cid := put.CID
	require.False(t, put.Encrypted, "public_archival blob must be stored unencrypted")

	// The donor serves the EXACT bytes the coordinator imported (the stored
	// plaintext envelope); AddDeterministic hashed them to cid.
	envBytes := make([]byte, len(plaintext))
	copy(envBytes, plaintext)

	_, err = svc.Resolve(ctx, cid)
	require.ErrorIs(t, err, storage.ErrStagingNotVisible, "staging blob must not be visible")
	require.Equal(t, 0, hook.count(), "OnCommitted must NOT fire for a staging blob")

	// =======================================================================
	// Seed 2 acked sourceable donor holders, each backed by a REAL read-source
	// server. Their source_nebula_addr is the server's loopback mTLS addr.
	// =======================================================================
	const generation = int64(1)
	envSize := int64(len(envBytes))
	asg1, asg2 := uuid.New(), uuid.New()

	donor1 := startDonor(t, caCertPEM, donorPub, cid, envBytes, asg1, generation, envSize)
	donor2 := startDonor(t, caCertPEM, donorPub, cid, envBytes, asg2, generation, envSize)
	defer donor1.close()
	defer donor2.close()

	seedSourceableNode(t, ctx, pool, donor1.nodeID, donor1.addr)
	seedSourceableNode(t, ctx, pool, donor2.nodeID, donor2.addr)
	seedAckedHolder(t, ctx, pool, cid, donor1.nodeID, asg1, generation)
	seedAckedHolder(t, ctx, pool, cid, donor2.nodeID, asg2, generation)

	// =======================================================================
	// STEP 2: ReconcileOnce ⇒ committed; OnCommitted fires exactly once; visible.
	// =======================================================================
	reconciler.ReconcileOnce(ctx)

	cs, err := gen.New(pool).GetCommitState(ctx, cid)
	require.NoError(t, err)
	require.Equal(t, gen.BlobCommitStateCommitted, cs, "blob must commit once quorum is met")
	require.Equal(t, 1, hook.count(), "OnCommitted must fire exactly once after commit")

	view, err := svc.Resolve(ctx, cid)
	require.NoError(t, err, "committed blob must be visible")
	require.False(t, view.Encrypted)

	// =======================================================================
	// STEP 3: PruneOnce ⇒ origin copy unpinned (sourceable=2 >= floor=2).
	// =======================================================================
	c, err := gocid.Decode(cid)
	require.NoError(t, err)
	has, err := backend.Has(ctx, c)
	require.NoError(t, err)
	require.True(t, has, "origin copy must be present before pruning")

	pruner.PruneOnce(ctx)

	has, err = backend.Has(ctx, c)
	require.NoError(t, err)
	require.False(t, has, "origin copy must be unpinned after pruning at/above floor")

	// =======================================================================
	// STEP 4: cold OpenBytes ⇒ REAL donor fetch over mTLS, verify-before-serve,
	// correct bytes; 2nd OpenBytes is a local cache hit (donor not re-hit).
	// =======================================================================
	rc, err := svc.OpenBytes(ctx, view)
	require.NoError(t, err, "cold read must fetch+verify from a donor")
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, plaintext, got, "donor-served bytes must round-trip to the original plaintext")

	servedAfterCold := donor1.pinner.served.Load() + donor2.pinner.served.Load()
	require.Equal(t, int64(1), servedAfterCold, "exactly one donor must have served the cold read")

	// Second read: now locally re-cached ⇒ a cache hit; no donor re-fetch.
	rc2, err := svc.OpenBytes(ctx, view)
	require.NoError(t, err)
	got2, err := io.ReadAll(rc2)
	require.NoError(t, err)
	require.NoError(t, rc2.Close())
	require.Equal(t, plaintext, got2)

	servedAfterWarm := donor1.pinner.served.Load() + donor2.pinner.served.Load()
	require.Equal(t, servedAfterCold, servedAfterWarm,
		"a warm (local cache hit) read must NOT re-hit any donor")

	// =======================================================================
	// STEP 5: blackhole BOTH donors AND make their nodes non-sourceable, then
	// evict the local cache copy ⇒ OpenBytes ⇒ ErrNoSourceableHolder (503), NOT
	// ErrBlobNotFound.
	// =======================================================================
	donor1.close()
	donor2.close()
	// Remove the read-source capability so no holder is selectable; this also
	// guards against any residual local copy by forcing the donor path.
	_, err = pool.Exec(ctx,
		`UPDATE nodes SET advertised_capabilities = ARRAY[]::text[] WHERE id = ANY($1)`,
		[]pgtype.UUID{
			{Bytes: donor1.nodeID, Valid: true},
			{Bytes: donor2.nodeID, Valid: true},
		})
	require.NoError(t, err)

	// Evict the locally re-cached copy so the read is forced down the donor path.
	require.NoError(t, backend.Unpin(ctx, c))

	_, err = svc.OpenBytes(ctx, view)
	require.ErrorIs(t, err, storage.ErrNoSourceableHolder,
		"with no reachable sourceable holder, OpenBytes must return the 503 sentinel")
	require.NotErrorIs(t, err, storage.ErrBlobNotFound,
		"an unavailable (but existing) blob must NOT be reported as not-found")
}
