package coordinator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// TestRotationCutover exercises the M2 downtime cert-rotation semantics through
// the handlers (design § Cert rotation, exit criterion #3): a node registers
// with cert A; the operator activates a replacement cert B (same node_id, new
// key ⇒ new fingerprint) by swapping the stored fingerprint; the OLD cert A then
// fails closed (fingerprint_mismatch 403) while the NEW cert B is accepted — and
// node_id is unchanged throughout (rotation must never look like a second node).
func TestRotationCutover(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	id := uuid.New()
	certA := issuedClient(t, caPEM, caKeyPEM, id)
	certB := issuedClient(t, caPEM, caKeyPEM, id) // same node_id URI SAN, fresh key ⇒ different fingerprint

	// Register with cert A.
	body, _ := json.Marshal(wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, certA))
	if w.Code != http.StatusCreated {
		t.Fatalf("register A = %d (%s)", w.Code, w.Body)
	}

	// Operator activates rotation: stored fingerprint → cert B's (DB-direct, the
	// novactl node rotate-cert path).
	if _, err := s.q.RotateNodeCert(t.Context(), gen.RotateNodeCertParams{
		ID:                        pgUUIDFrom(id),
		FederationCertFingerprint: transport.FingerprintDER(certB),
	}); err != nil {
		t.Fatal(err)
	}

	// Old cert A now fails closed.
	wA := httptest.NewRecorder()
	s.handleHeartbeat(wA, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), certA))
	if wA.Code != http.StatusForbidden {
		t.Fatalf("old cert heartbeat = %d, want 403", wA.Code)
	}

	// New cert B is accepted.
	wB := httptest.NewRecorder()
	s.handleHeartbeat(wB, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), certB))
	if wB.Code != http.StatusOK {
		t.Fatalf("new cert heartbeat = %d (%s), want 200", wB.Code, wB.Body)
	}

	// node_id unchanged: same row, now bound to fingerprint B.
	node, err := s.q.GetNodeByID(t.Context(), pgUUIDFrom(id))
	if err != nil {
		t.Fatal(err)
	}
	if node.FederationCertFingerprint != transport.FingerprintDER(certB) {
		t.Fatalf("stored fingerprint is not cert B's after rotation")
	}
}
