package coordinator

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// issuedClient returns a parsed client leaf for nodeID signed by caPEM/caKeyPEM.
func issuedClient(t *testing.T, caPEM, caKeyPEM []byte, nodeID uuid.UUID) *x509.Certificate {
	t.Helper()
	cliPEM, _, err := ca.IssueClientCert(caPEM, caKeyPEM, nodeID, "donor")
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(cliPEM)
	if blk == nil {
		t.Fatal("no PEM in issued client cert")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// reqWithCert builds a request whose TLS state presents leaf as the peer cert.
func reqWithCert(method, path string, body []byte, leaf *x509.Certificate) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	return r
}

func newTestServer(t *testing.T) (*Server, []byte, []byte) {
	t.Helper()
	pool := dbtest.New(t, t.Context())
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	s := New(gen.New(pool), Config{
		Timers: wire.ConfigUpdates{HeartbeatIntervalSeconds: 300, PinsPollIntervalSeconds: 600, MaxPinConcurrency: 16},
	})
	return s, caPEM, caKeyPEM
}

func TestRegisterFirstAndIdempotent(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	id := uuid.New()
	leaf := issuedClient(t, caPEM, caKeyPEM, id)
	body, _ := json.Marshal(wire.RegisterRequest{
		SupportedProtocols:        []string{wire.ProtocolV1},
		Capabilities:              []string{},
		FederationCertFingerprint: transport.FingerprintDER(leaf),
		DisplayName:               "donor-a",
		CapacityBytes:             1 << 40,
	})

	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusCreated {
		t.Fatalf("first register = %d (%s)", w.Code, w.Body)
	}
	var resp wire.RegisterResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.NodeID != id.String() || resp.SelectedProtocol != wire.ProtocolV1 {
		t.Fatalf("resp = %+v", resp)
	}

	w2 := httptest.NewRecorder()
	s.handleRegister(w2, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w2.Code != http.StatusOK {
		t.Fatalf("re-register = %d", w2.Code)
	}
}

func TestRegisterIncompatibleProtocol(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	leaf := issuedClient(t, caPEM, caKeyPEM, uuid.New())
	body, _ := json.Marshal(wire.RegisterRequest{SupportedProtocols: []string{"fed/v2"}})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	var e wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &e)
	if e.Code != "incompatible_protocol" {
		t.Fatalf("code = %q", e.Code)
	}
}

func TestRegisterMissingCapabilityFailsClosed(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	s.cfg.RequiredCapabilities = []string{wire.CapPinChangeLog}
	leaf := issuedClient(t, caPEM, caKeyPEM, uuid.New())
	body, _ := json.Marshal(wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}, Capabilities: []string{}})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
}
