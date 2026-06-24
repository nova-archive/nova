package coordinator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// TestRegisterPersistsSourceAddr verifies that when a donor sends SourceNebulaAddr
// in its RegisterRequest, the coordinator writes it to nodes.source_nebula_addr.
func TestRegisterPersistsSourceAddr(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	id := uuid.New()
	leaf := issuedClient(t, caPEM, caKeyPEM, id)
	const addr = "10.42.0.3:9200"
	body, _ := json.Marshal(wire.RegisterRequest{
		SupportedProtocols:        []string{wire.ProtocolV1},
		Capabilities:              []string{wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer, wire.CapReadSource},
		FederationCertFingerprint: transport.FingerprintDER(leaf),
		DisplayName:               "read-source-donor",
		CapacityBytes:             1 << 40,
		SourceNebulaAddr:          addr,
	})

	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusCreated {
		t.Fatalf("register = %d (%s)", w.Code, w.Body)
	}

	// Verify the addr was persisted.
	node, err := s.q.GetNodeByID(t.Context(), pgUUIDFrom(id))
	if err != nil {
		t.Fatalf("GetNodeByID: %v", err)
	}
	if !node.SourceNebulaAddr.Valid || node.SourceNebulaAddr.String != addr {
		t.Fatalf("source_nebula_addr = %+v, want %q", node.SourceNebulaAddr, addr)
	}
	if !slices.Contains(node.AdvertisedCapabilities, wire.CapReadSource) {
		t.Fatalf("advertised_capabilities %v missing %q", node.AdvertisedCapabilities, wire.CapReadSource)
	}
}

// TestHeartbeatUpdatesSourceAddr verifies that a heartbeat carrying SourceNebulaAddr
// updates the stored value, and that an empty heartbeat does NOT wipe the existing one.
func TestHeartbeatUpdatesSourceAddr(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	id := uuid.New()
	leaf := issuedClient(t, caPEM, caKeyPEM, id)
	const addr = "10.42.0.7:9200"

	// Register first (with addr).
	regBody, _ := json.Marshal(wire.RegisterRequest{
		SupportedProtocols:        []string{wire.ProtocolV1},
		Capabilities:              []string{wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer, wire.CapReadSource},
		FederationCertFingerprint: transport.FingerprintDER(leaf),
		SourceNebulaAddr:          addr,
	})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", regBody, leaf))
	if w.Code != http.StatusCreated {
		t.Fatalf("register = %d (%s)", w.Code, w.Body)
	}

	// Heartbeat with EMPTY addr — must not wipe the stored value.
	hbBody, _ := json.Marshal(wire.HeartbeatRequest{FreeBytes: 1000, StoredBytes: 500})
	w2 := httptest.NewRecorder()
	s.handleHeartbeat(w2, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", hbBody, leaf))
	if w2.Code != http.StatusOK {
		t.Fatalf("heartbeat (empty addr) = %d (%s)", w2.Code, w2.Body)
	}

	node, err := s.q.GetNodeByID(t.Context(), pgUUIDFrom(id))
	if err != nil {
		t.Fatalf("GetNodeByID after empty-addr heartbeat: %v", err)
	}
	if !node.SourceNebulaAddr.Valid || node.SourceNebulaAddr.String != addr {
		t.Fatalf("source_nebula_addr wiped: got %+v, want %q", node.SourceNebulaAddr, addr)
	}

	// Heartbeat WITH a new addr — must update.
	const newAddr = "10.42.0.8:9200"
	hbBody2, _ := json.Marshal(wire.HeartbeatRequest{SourceNebulaAddr: newAddr})
	w3 := httptest.NewRecorder()
	s.handleHeartbeat(w3, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", hbBody2, leaf))
	if w3.Code != http.StatusOK {
		t.Fatalf("heartbeat (new addr) = %d (%s)", w3.Code, w3.Body)
	}

	node2, err := s.q.GetNodeByID(t.Context(), pgUUIDFrom(id))
	if err != nil {
		t.Fatalf("GetNodeByID after new-addr heartbeat: %v", err)
	}
	if !node2.SourceNebulaAddr.Valid || node2.SourceNebulaAddr.String != newAddr {
		t.Fatalf("source_nebula_addr not updated: got %+v, want %q", node2.SourceNebulaAddr, newAddr)
	}
}

// TestCapabilityNegotiationUnaffected ensures a donor advertising only the M4
// required caps (without read-source/v1) still registers successfully (201). The
// read-source/v1 capability must NOT be in RequiredCapabilities.
func TestCapabilityNegotiationUnaffected(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	// Set M4-era required caps — must NOT include CapReadSource.
	s.cfg.RequiredCapabilities = []string{
		wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer,
	}
	id := uuid.New()
	leaf := issuedClient(t, caPEM, caKeyPEM, id)
	body, _ := json.Marshal(wire.RegisterRequest{
		SupportedProtocols:        []string{wire.ProtocolV1},
		Capabilities:              []string{wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer},
		FederationCertFingerprint: transport.FingerprintDER(leaf),
	})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.Bytes())
	}

	// Verify read-source/v1 is NOT in required_capabilities for this node.
	node, err := s.q.GetNodeByID(t.Context(), pgUUIDFrom(id))
	if err != nil {
		t.Fatalf("GetNodeByID: %v", err)
	}
	if slices.Contains(node.RequiredCapabilities, wire.CapReadSource) {
		t.Fatalf("required_capabilities should not include %q, got %v", wire.CapReadSource, node.RequiredCapabilities)
	}
	// And no source addr was set (donor didn't send one).
	if node.SourceNebulaAddr.Valid && node.SourceNebulaAddr.String != "" {
		t.Fatalf("source_nebula_addr should be empty, got %q", node.SourceNebulaAddr.String)
	}

	_ = gen.Node{} // ensure models compile
}

