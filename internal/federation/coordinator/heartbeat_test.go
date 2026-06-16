package coordinator

import (
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func registerOK(t *testing.T, s *Server, caPEM, caKeyPEM []byte, id uuid.UUID) *x509.Certificate {
	t.Helper()
	leaf := issuedClient(t, caPEM, caKeyPEM, id)
	body, _ := json.Marshal(wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusCreated {
		t.Fatalf("setup register = %d (%s)", w.Code, w.Body)
	}
	return leaf
}

func TestHeartbeatUpdatesAndConfig(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	body, _ := json.Marshal(wire.HeartbeatRequest{FreeBytes: 100, StoredBytes: 200})
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", body, leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d (%s)", w.Code, w.Body)
	}
	var resp wire.HeartbeatResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ConfigUpdates == nil || resp.ConfigUpdates.HeartbeatIntervalSeconds != 300 {
		t.Fatalf("config_updates = %+v", resp.ConfigUpdates)
	}
	if resp.CurrentEpoch != 0 || resp.RepairTokenPublicKey != "" {
		t.Fatalf("epoch/repair-key = %d/%q", resp.CurrentEpoch, resp.RepairTokenPublicKey)
	}
}

func TestHeartbeatUnregistered403(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	leaf := issuedClient(t, caPEM, caKeyPEM, uuid.New()) // never registered
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), leaf))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestRevokedCertRejected(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	id := uuid.New()
	registerOK(t, s, caPEM, caKeyPEM, id)
	if _, err := s.q.RevokeNode(t.Context(), pgUUIDFrom(id)); err != nil {
		t.Fatal(err)
	}
	leaf := issuedClient(t, caPEM, caKeyPEM, id)
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), leaf))
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoked heartbeat = %d", w.Code)
	}
	_ = gen.NodeStatusRevoked
}
