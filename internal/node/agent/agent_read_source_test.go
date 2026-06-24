package agent

import (
	"context"
	"crypto/ed25519"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

// captureClient captures the last register/heartbeat requests.
type captureClient struct {
	lastRegReq atomic.Pointer[wire.RegisterRequest]
	lastHBReq  atomic.Pointer[wire.HeartbeatRequest]
}

func (c *captureClient) Register(_ context.Context, req wire.RegisterRequest) (wire.RegisterResponse, error) {
	c.lastRegReq.Store(&req)
	return wire.RegisterResponse{NodeID: "n1", SelectedProtocol: wire.ProtocolV1}, nil
}
func (c *captureClient) Heartbeat(_ context.Context, req wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	c.lastHBReq.Store(&req)
	return wire.HeartbeatResponse{}, nil
}
func (c *captureClient) GetChanges(context.Context, int64) (wire.ChangesResponse, error) {
	return wire.ChangesResponse{}, nil
}
func (c *captureClient) GetSnapshot(context.Context, string, int64) (wire.SnapshotResponse, error) {
	return wire.SnapshotResponse{}, nil
}
func (c *captureClient) Ack(context.Context, string, wire.Ack) error   { return nil }
func (c *captureClient) Fail(context.Context, string, wire.Fail) error { return nil }

// TestDonorAdvertisesReadSource verifies that when SourceNebulaAddr is set in config,
// the agent includes wire.CapReadSource in the register request and populates
// SourceNebulaAddr in both register and heartbeat requests.
func TestDonorAdvertisesReadSource(t *testing.T) {
	const addr = "10.100.0.5:9200"
	cfg := &nodeconfig.Config{
		BandwidthBudgetBytesPerDay: 1,
		SourceNebulaAddr:           addr,
	}
	cc := &captureClient{}
	a := New(cfg,
		state.NewFileRegistrationStore(t.TempDir()),
		state.NewFileStore(t.TempDir()),
		state.NewFileAssignmentStore(t.TempDir()),
		cc, 20*time.Millisecond, time.Hour,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	regReq := cc.lastRegReq.Load()
	if regReq == nil {
		t.Fatal("no register request captured")
	}
	hasReadSource := false
	for _, c := range regReq.Capabilities {
		if c == wire.CapReadSource {
			hasReadSource = true
			break
		}
	}
	if !hasReadSource {
		t.Fatalf("register capabilities %v missing %q", regReq.Capabilities, wire.CapReadSource)
	}
	if regReq.SourceNebulaAddr != addr {
		t.Fatalf("register SourceNebulaAddr = %q, want %q", regReq.SourceNebulaAddr, addr)
	}

	hbReq := cc.lastHBReq.Load()
	if hbReq == nil {
		t.Fatal("no heartbeat request captured")
	}
	if hbReq.SourceNebulaAddr != addr {
		t.Fatalf("heartbeat SourceNebulaAddr = %q, want %q", hbReq.SourceNebulaAddr, addr)
	}
}

// pubkeyClient serves a fixed RepairTokenPublicKey on every heartbeat.
type pubkeyClient struct {
	captureClient
	wireKey string
}

func (c *pubkeyClient) Heartbeat(_ context.Context, req wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	c.lastHBReq.Store(&req)
	return wire.HeartbeatResponse{RepairTokenPublicKey: c.wireKey}, nil
}

// fakeSink records the last pubkey the agent installed.
type fakeSink struct {
	last atomic.Pointer[ed25519.PublicKey]
}

func (s *fakeSink) Set(pub ed25519.PublicKey) { p := pub; s.last.Store(&p) }

// TestAgentCapturesRepairPubKey verifies the heartbeat loop decodes
// HeartbeatResponse.RepairTokenPublicKey and feeds it to the injected sink
// (D-M4.1-18), so the source server learns the coordinator's verify key.
func TestAgentCapturesRepairPubKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cc := &pubkeyClient{wireKey: wire.EncodePublicKey(pub)}
	sink := &fakeSink{}
	cfg := &nodeconfig.Config{BandwidthBudgetBytesPerDay: 1}
	a := New(cfg,
		state.NewFileRegistrationStore(t.TempDir()),
		state.NewFileStore(t.TempDir()),
		state.NewFileAssignmentStore(t.TempDir()),
		cc, 20*time.Millisecond, time.Hour,
	)
	WithPubKeySink(a, sink)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	got := sink.last.Load()
	if got == nil {
		t.Fatal("sink never received a pubkey")
	}
	if !got.Equal(pub) {
		t.Fatal("captured pubkey does not match the heartbeat key")
	}
}
