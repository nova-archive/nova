package source

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/federation/replay"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/state"
)

// fakeAuditBlockReader implements AuditBlockReader for tests. Any CID not in
// blocks returns an error (simulating BlockGetLocal's "not present" path).
type fakeAuditBlockReader struct {
	blocks map[string][]byte
}

func (f *fakeAuditBlockReader) BlockGetLocal(_ context.Context, cid string) ([]byte, error) {
	b, ok := f.blocks[cid]
	if !ok {
		return nil, errors.New("block not found")
	}
	return b, nil
}

// auditFakes groups all fake deps needed by newAuditTestServer.
type auditFakes struct {
	progress map[string]state.Progress
	pinned   map[string]bool
	blocks   map[string][]byte
	budgetOK bool
}

// newAuditTestServer builds a *Server with audit deps wired from f. The
// blob-handler deps (pubkey, replay, blob budget) are left as stubbed-out
// fail-closed values — they are never reached by audit tests.
func newAuditTestServer(t *testing.T, f auditFakes) *Server {
	t.Helper()
	if f.pinned == nil {
		f.pinned = map[string]bool{}
	}
	if f.blocks == nil {
		f.blocks = map[string][]byte{}
	}
	if f.progress == nil {
		f.progress = map[string]state.Progress{}
	}
	return NewServer(Deps{
		Pinner:      &fakePinner{has: f.pinned},
		Budget:      &fakeBudget{allow: false}, // blob path — never reached
		PubKey:      &fakePubkey{ok: false},    // blob path — never reached
		Progress:    &fakeProgress{m: f.progress},
		ReplayCache: replay.New(),
		AuditBlocks: &fakeAuditBlockReader{blocks: f.blocks},
		AuditBudget: &fakeBudget{allow: f.budgetOK},
	})
}

// coordinatorPeer returns a self-signed cert whose URI SAN identifies a coordinator.
func coordinatorPeer(t *testing.T) *x509.Certificate {
	t.Helper()
	return makeCert(t, "coordinator")
}

// nodePeer returns a self-signed cert whose URI SAN identifies a donor node.
func nodePeer(t *testing.T) *x509.Certificate {
	t.Helper()
	return makeCert(t, "node")
}

// doAudit marshals body as JSON, POSTs to /fed/v1/audit/challenge with peer as
// the TLS peer cert, and returns the response recorder.
func doAudit(t *testing.T, s *Server, peer *x509.Certificate, body wire.AuditChallenge) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/fed/v1/audit/challenge", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if peer != nil {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// --- tests ------------------------------------------------------------------

func TestAuditChallengePassReturnsBlockBytes(t *testing.T) {
	s := newAuditTestServer(t, auditFakes{
		progress: map[string]state.Progress{"blob": {State: state.ProgressAckDelivered, AssignmentID: "a1", Generation: 7, ByteSize: 100}},
		pinned:   map[string]bool{"blob": true},
		blocks:   map[string][]byte{"blk": bytes.Repeat([]byte{0xAB}, 64)},
		budgetOK: true,
	})
	body := wire.AuditChallenge{ChallengeID: "c1", ChallengeKind: "block_hash", BlobCID: "blob",
		AssignmentID: "a1", Generation: 7, BlockIndex: 0, BlockCID: "blk", BlockSize: 64, Nonce: "n"}
	rec := doAudit(t, s, coordinatorPeer(t), body)
	if rec.Code != 200 {
		t.Fatalf("code %d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), bytes.Repeat([]byte{0xAB}, 64)) {
		t.Fatal("wrong bytes")
	}
}

func TestAuditChallengeRejectsRoleNode(t *testing.T) {
	s := newAuditTestServer(t, auditFakes{})
	rec := doAudit(t, s, nodePeer(t), wire.AuditChallenge{BlobCID: "blob", BlockCID: "blk"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("RoleNode must be 403, got %d", rec.Code)
	}
}

func TestAuditChallenge404WhenBlockNotLocal(t *testing.T) {
	s := newAuditTestServer(t, auditFakes{
		progress: map[string]state.Progress{"blob": {State: state.ProgressAckDelivered, AssignmentID: "a1", Generation: 7, ByteSize: 100}},
		pinned:   map[string]bool{"blob": true},
		blocks:   map[string][]byte{}, // BlockGetLocal -> not found
		budgetOK: true,
	})
	body := wire.AuditChallenge{ChallengeID: "c", ChallengeKind: "block_hash", BlobCID: "blob", AssignmentID: "a1", Generation: 7, BlockCID: "blk", BlockSize: 64, Nonce: "n"}
	if doAudit(t, s, coordinatorPeer(t), body).Code != 404 {
		t.Fatal("missing block must be 404")
	}
}

func TestAuditChallengeAssignmentMismatchFails(t *testing.T) {
	s := newAuditTestServer(t, auditFakes{
		progress: map[string]state.Progress{"blob": {State: state.ProgressAckDelivered, AssignmentID: "OTHER", Generation: 7, ByteSize: 100}},
		pinned:   map[string]bool{"blob": true}, blocks: map[string][]byte{"blk": {1}}, budgetOK: true,
	})
	body := wire.AuditChallenge{ChallengeID: "c", ChallengeKind: "block_hash", BlobCID: "blob", AssignmentID: "a1", Generation: 7, BlockCID: "blk", BlockSize: 1, Nonce: "n"}
	if doAudit(t, s, coordinatorPeer(t), body).Code != 404 {
		t.Fatal("assignment mismatch must fail (404)")
	}
}

func TestAuditChallengeBudgetExhaustedSignalsSkip(t *testing.T) {
	s := newAuditTestServer(t, auditFakes{
		progress: map[string]state.Progress{"blob": {State: state.ProgressAckDelivered, AssignmentID: "a1", Generation: 7, ByteSize: 100}},
		pinned:   map[string]bool{"blob": true}, blocks: map[string][]byte{"blk": {1}}, budgetOK: false,
	})
	body := wire.AuditChallenge{ChallengeID: "c", ChallengeKind: "block_hash", BlobCID: "blob", AssignmentID: "a1", Generation: 7, BlockCID: "blk", BlockSize: 1, Nonce: "n"}
	if doAudit(t, s, coordinatorPeer(t), body).Code != http.StatusTooManyRequests {
		t.Fatal("budget exhausted must be 429 (->skip)")
	}
}
