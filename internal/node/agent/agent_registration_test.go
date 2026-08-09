package agent

import (
	"context"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

// TestFirstHeartbeatIsImmediate pins the P2-M7.2 D-M7.2-7 fix. Before it, the
// first heartbeat waited a full interval (default 300s in production), so a
// freshly registered donor was invisible to the coordinator's liveness view
// for five minutes. syncOnce was already immediate; the heartbeat was not.
func TestFirstHeartbeatIsImmediate(t *testing.T) {
	cfg := &nodeconfig.Config{BandwidthBudgetBytesPerDay: 1}
	store := state.NewFileRegistrationStore(t.TempDir())
	fc := &fakeClient{regResp: wire.RegisterResponse{NodeID: "n1", SelectedProtocol: wire.ProtocolV1}}
	// An hour-long heartbeat interval: only an immediate call can satisfy this.
	a := New(cfg, store, state.NewFileStore(t.TempDir()), state.NewFileAssignmentStore(t.TempDir()), fc, time.Hour, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	if got := fc.heartbeats.Load(); got < 1 {
		t.Fatalf("heartbeats = %d, want an immediate first heartbeat before the interval elapsed", got)
	}
}

// TestOnRegisteredFiresOnceAfterDurableSave proves the read-source start hook
// fires after the registration is durable, so cmd/node can start read-source
// in-process instead of deferring it to the next boot.
func TestOnRegisteredFiresOnceAfterDurableSave(t *testing.T) {
	store := state.NewFileRegistrationStore(t.TempDir())
	fc := &fakeClient{regResp: wire.RegisterResponse{NodeID: "n1", SelectedProtocol: wire.ProtocolV1}}
	a := New(&nodeconfig.Config{BandwidthBudgetBytesPerDay: 1}, store,
		state.NewFileStore(t.TempDir()), state.NewFileAssignmentStore(t.TempDir()), fc, time.Hour, time.Hour)

	var got []string
	var sawDurable bool
	a.SetOnRegistered(func(id string) {
		got = append(got, id)
		if _, ok, _ := store.LoadRegistration(context.Background()); ok {
			sawDurable = true
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	if len(got) != 1 || got[0] != "n1" {
		t.Fatalf("hook calls = %v, want exactly one call with n1", got)
	}
	if !sawDurable {
		t.Fatal("registration must already be durable when the hook fires")
	}
}

// TestOnRegisteredDoesNotFireForAnExistingRegistration ensures a restart does
// not re-announce: cmd/node starts read-source directly in that case.
func TestOnRegisteredDoesNotFireForAnExistingRegistration(t *testing.T) {
	store := state.NewFileRegistrationStore(t.TempDir())
	_ = store.SaveRegistration(context.Background(), state.Registration{NodeID: "n9"})
	fc := &fakeClient{}
	a := New(&nodeconfig.Config{BandwidthBudgetBytesPerDay: 1}, store,
		state.NewFileStore(t.TempDir()), state.NewFileAssignmentStore(t.TempDir()), fc, time.Hour, time.Hour)

	fired := 0
	a.SetOnRegistered(func(string) { fired++ })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	if fired != 0 {
		t.Fatalf("hook fired %d times for an existing registration, want 0", fired)
	}
}
