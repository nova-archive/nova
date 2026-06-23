package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// TestChangesPopulatesSourceForPendingAssign verifies that handleChanges sets
// PinChange.Source for a pending assign row when a signer is configured.
func TestChangesPopulatesSourceForPendingAssign(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	signer, err := tokens.NewSignerFromSeed(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.RepairTokenTTL = time.Hour
	s.SetSourceDeps(signer, fakeBackendFor("unused", nil), time.Now().Add(-time.Minute))

	seedBlob(t, ctx, pool, "bafy-src-test", 42)
	assignViaSeam(t, ctx, pool, "bafy-src-test", id)

	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("changes: status %d body %s", w.Code, w.Body)
	}
	var resp wire.ChangesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Changes) != 1 {
		t.Fatalf("expected 1 change, got %d: %+v", len(resp.Changes), resp.Changes)
	}
	ch := resp.Changes[0]
	if ch.Kind != wire.ChangeKindAssign {
		t.Fatalf("expected assign kind, got %q", ch.Kind)
	}
	if ch.Source == nil {
		t.Fatalf("expected Source to be set, got nil")
	}

	// Verify the minted token is cryptographically valid and has correct claims.
	pub, err := wire.DecodePublicKey(signer.PublicKeyWire())
	if err != nil {
		t.Fatalf("decode pubkey: %v", err)
	}
	claims, err := wire.Verify(pub, ch.Source.Token, time.Now())
	if err != nil {
		t.Fatalf("token verify failed: %v", err)
	}
	if claims.DestNodeID != id.String() {
		t.Fatalf("dest_node_id: got %q, want %q", claims.DestNodeID, id.String())
	}
	if claims.CID != "bafy-src-test" {
		t.Fatalf("cid: got %q, want %q", claims.CID, "bafy-src-test")
	}
}

// TestMintedTokenAcceptedImmediatelyAfterBoot is a regression test for D-M4-9:
// a token minted right after boot must NOT have a not_before earlier than
// source_boot_time, or the blob endpoint would reject it.
func TestMintedTokenAcceptedImmediatelyAfterBoot(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	signer, err := tokens.NewSignerFromSeed(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	// bootTime is right now — simulates a fresh startup.
	boot := time.Now()

	body := []byte("boot-regression-data")
	cidStr := mkCID(t, body)

	s.cfg.RepairTokenTTL = time.Hour
	s.SetSourceDeps(signer, fakeBackendFor(cidStr, body), boot)

	insertBlobRow(t, pool, cidStr, int64(len(body)))
	assignViaSeam(t, ctx, pool, cidStr, id)

	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("changes: status %d body %s", w.Code, w.Body)
	}
	var resp wire.ChangesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Changes) == 0 || resp.Changes[0].Source == nil {
		t.Fatalf("expected source-bearing assign, got %+v", resp.Changes)
	}
	src := resp.Changes[0].Source

	pub, err := wire.DecodePublicKey(signer.PublicKeyWire())
	if err != nil {
		t.Fatalf("decode pubkey: %v", err)
	}
	claims, err := wire.Verify(pub, src.Token, time.Now())
	if err != nil {
		t.Fatalf("token verify (fresh boot): %v", err)
	}
	if claims.NotBefore < boot.Unix() {
		t.Fatalf("not_before %d < source_boot_time %d (would self-reject)", claims.NotBefore, boot.Unix())
	}

	// The blob endpoint must also accept the minted token end-to-end.
	blobReq := reqWithCert(http.MethodGet, "/fed/v1/blob/"+cidStr, nil, leaf)
	blobReq.Header.Set("X-Nova-Repair-Token", src.Token)
	bw := httptest.NewRecorder()
	s.mux().ServeHTTP(bw, blobReq)
	if bw.Code != http.StatusOK {
		t.Fatalf("fresh-boot token rejected by blob endpoint: %d body %s", bw.Code, bw.Body)
	}
}

// TestHeartbeatReturnsPublicKey verifies that a configured signer causes
// handleHeartbeat to include RepairTokenPublicKey in the response.
func TestHeartbeatReturnsPublicKey(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	signer, err := tokens.NewSignerFromSeed(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSourceDeps(signer, nil, time.Now())

	body, _ := json.Marshal(wire.HeartbeatRequest{FreeBytes: 100, StoredBytes: 200})
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", body, leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: status %d body %s", w.Code, w.Body)
	}
	var resp wire.HeartbeatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.RepairTokenPublicKey != signer.PublicKeyWire() {
		t.Fatalf("pubkey: got %q, want %q", resp.RepairTokenPublicKey, signer.PublicKeyWire())
	}
}

// TestRegisterRequiresBlobTransfer verifies that a coordinator configured with
// RequiredCapabilities including wire.CapBlobTransfer:
//   - rejects a donor advertising only pin-change-log/v1 + snapshot/v1 (missing blob-transfer/v1)
//   - accepts a donor that also advertises blob-transfer/v1
func TestRegisterRequiresBlobTransfer(t *testing.T) {
	s, _, caPEM, caKeyPEM := newTestServerPool(t)
	s.cfg.RequiredCapabilities = []string{wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer}

	// Donor WITHOUT blob-transfer/v1 → must be rejected with 400 missing_capability.
	leafWithout := issuedClient(t, caPEM, caKeyPEM, uuid.New())
	bodyWithout, _ := json.Marshal(wire.RegisterRequest{
		SupportedProtocols: []string{wire.ProtocolV1},
		Capabilities:       []string{wire.CapPinChangeLog, wire.CapSnapshot},
	})
	wWithout := httptest.NewRecorder()
	s.handleRegister(wWithout, reqWithCert(http.MethodPost, "/fed/v1/register", bodyWithout, leafWithout))
	if wWithout.Code != http.StatusBadRequest {
		t.Fatalf("without blob-transfer: status %d (want 400) body %s", wWithout.Code, wWithout.Body)
	}
	var errResp wire.ErrorResponse
	if err := json.Unmarshal(wWithout.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v", err)
	}
	if errResp.Code != "missing_capability" {
		t.Fatalf("wrong error code: %q (want missing_capability)", errResp.Code)
	}

	// Donor WITH blob-transfer/v1 → must be accepted with 201.
	leafWith := issuedClient(t, caPEM, caKeyPEM, uuid.New())
	bodyWith, _ := json.Marshal(wire.RegisterRequest{
		SupportedProtocols: []string{wire.ProtocolV1},
		Capabilities:       []string{wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer},
	})
	wWith := httptest.NewRecorder()
	s.handleRegister(wWith, reqWithCert(http.MethodPost, "/fed/v1/register", bodyWith, leafWith))
	if wWith.Code != http.StatusCreated {
		t.Fatalf("with blob-transfer: status %d (want 201) body %s", wWith.Code, wWith.Body)
	}
}
