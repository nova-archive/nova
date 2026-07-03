package coordinator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// TestRegisterFailureHookFires verifies the P2-M7 (D-M7-1) observability seam:
// the nil-safe OnRegisterFailure hook fires with the wire error code on each
// register rejection. The default-nil case is covered by every other register
// test in this package (they run with no hook set).
func TestRegisterFailureHookFires(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	var got []string
	s.cfg.OnRegisterFailure = func(reason string) { got = append(got, reason) }
	s.cfg.RequiredCapabilities = []string{wire.CapPinChangeLog, wire.CapSnapshot}
	leaf := issuedClient(t, caPEM, caKeyPEM, uuid.New())

	body, _ := json.Marshal(wire.RegisterRequest{SupportedProtocols: []string{"fed/v2"}})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("protocol-mismatch status = %d", w.Code)
	}

	body2, _ := json.Marshal(wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}, Capabilities: []string{}})
	w2 := httptest.NewRecorder()
	s.handleRegister(w2, reqWithCert(http.MethodPost, "/fed/v1/register", body2, leaf))
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("missing-capability status = %d", w2.Code)
	}

	if len(got) != 2 || got[0] != "incompatible_protocol" || got[1] != "missing_capability" {
		t.Fatalf("hook observed %v, want [incompatible_protocol missing_capability]", got)
	}
}
