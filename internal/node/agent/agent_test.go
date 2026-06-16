package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

type fakeClient struct {
	registers  atomic.Int32
	heartbeats atomic.Int32
	regResp    wire.RegisterResponse
	hbErr      error
}

func (f *fakeClient) Register(context.Context, wire.RegisterRequest) (wire.RegisterResponse, error) {
	f.registers.Add(1)
	return f.regResp, nil
}
func (f *fakeClient) Heartbeat(context.Context, wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	f.heartbeats.Add(1)
	// No cadence change: the test keeps the fast loop interval so it exercises
	// register-once + repeated-heartbeat, not the interval-change path.
	return wire.HeartbeatResponse{}, f.hbErr
}

func TestAgentRegistersOnceThenHeartbeats(t *testing.T) {
	cfg := &nodeconfig.Config{BandwidthBudgetBytesPerDay: 1}
	store := state.NewFileRegistrationStore(t.TempDir())
	fc := &fakeClient{regResp: wire.RegisterResponse{NodeID: "n1", SelectedProtocol: wire.ProtocolV1}}
	a := New(cfg, store, fc, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	if got := fc.registers.Load(); got != 1 {
		t.Fatalf("registers = %d, want 1", got)
	}
	if got := fc.heartbeats.Load(); got < 2 {
		t.Fatalf("heartbeats = %d, want >= 2", got)
	}
	if reg, ok, _ := store.LoadRegistration(ctx); !ok || reg.NodeID != "n1" {
		t.Fatalf("registration not persisted: %+v ok=%v", reg, ok)
	}
}

func TestAgentSkipsRegisterWhenAlreadyRegistered(t *testing.T) {
	store := state.NewFileRegistrationStore(t.TempDir())
	_ = store.SaveRegistration(context.Background(), state.Registration{NodeID: "n9"})
	fc := &fakeClient{}
	a := New(&nodeconfig.Config{BandwidthBudgetBytesPerDay: 1}, store, fc, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)
	if fc.registers.Load() != 0 {
		t.Fatalf("should not re-register, got %d", fc.registers.Load())
	}
}
