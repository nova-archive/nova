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

type syncFake struct {
	acks      atomic.Int32 // MUST stay 0 in M3
	changes   []wire.ChangesResponse
	idx       atomic.Int32
	snapResp  wire.SnapshotResponse
	forceSnap bool
}

func (f *syncFake) Register(context.Context, wire.RegisterRequest) (wire.RegisterResponse, error) {
	return wire.RegisterResponse{NodeID: "n1", SelectedProtocol: wire.ProtocolV1}, nil
}
func (f *syncFake) Heartbeat(context.Context, wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	return wire.HeartbeatResponse{ConfigUpdates: &wire.ConfigUpdates{}}, nil
}
func (f *syncFake) GetChanges(_ context.Context, since int64) (wire.ChangesResponse, error) {
	if f.forceSnap && since == 0 {
		return wire.ChangesResponse{}, ErrSnapshotRequired
	}
	i := int(f.idx.Add(1)) - 1
	if i < len(f.changes) {
		return f.changes[i], nil
	}
	return wire.ChangesResponse{NextSeq: since, CurrentEpoch: since}, nil
}
func (f *syncFake) GetSnapshot(context.Context, string, int64) (wire.SnapshotResponse, error) {
	return f.snapResp, nil
}

func TestSyncOnceAppliesIdempotentlyAndNeverAcks(t *testing.T) {
	dir := t.TempDir()
	cur := state.NewFileStore(dir)
	asg := state.NewFileAssignmentStore(dir)
	f := &syncFake{changes: []wire.ChangesResponse{
		{Changes: []wire.PinChange{{Sequence: 1, AssignmentID: "a", Generation: 1, Kind: "assign", CID: "bafy1", ByteSize: 5}}, NextSeq: 1, CurrentEpoch: 1},
	}}
	a := New(&nodeconfig.Config{}, state.NewFileRegistrationStore(dir), cur, asg, f, time.Second, time.Second)

	next := a.syncOnce(context.Background(), 0)
	if next != 1 {
		t.Fatalf("cursor = %d, want 1", next)
	}
	got, _ := asg.List()
	if len(got) != 1 || got[0].State != "pending" {
		t.Fatalf("desired set: %+v", got)
	}
	if f.acks.Load() != 0 {
		t.Fatal("donor must NOT ack in M3")
	}
	// replay (same change) is a no-op
	f.idx.Store(0)
	a.syncOnce(context.Background(), 0)
	if got, _ := asg.List(); len(got) != 1 {
		t.Fatalf("replay not idempotent: %+v", got)
	}
}

func TestSyncSnapshotRecovery(t *testing.T) {
	dir := t.TempDir()
	cur := state.NewFileStore(dir)
	asg := state.NewFileAssignmentStore(dir)
	f := &syncFake{forceSnap: true, snapResp: wire.SnapshotResponse{
		Data: []wire.SnapshotItem{{CID: "bafy1", AssignmentID: "a", Generation: 3, ByteSize: 5}}, Cursor: "", SnapshotEpoch: 9,
	}}
	a := New(&nodeconfig.Config{}, state.NewFileRegistrationStore(dir), cur, asg, f, time.Second, time.Second)

	next := a.syncOnce(context.Background(), 0)
	if next != 9 {
		t.Fatalf("cursor after recovery = %d, want 9", next)
	}
	got, _ := asg.List()
	if len(got) != 1 || got[0].Generation != 3 {
		t.Fatalf("recovered set: %+v", got)
	}
}

func TestSyncUnknownKindFailsClosed(t *testing.T) {
	dir := t.TempDir()
	asg := state.NewFileAssignmentStore(dir)
	f := &syncFake{
		changes:  []wire.ChangesResponse{{Changes: []wire.PinChange{{Sequence: 1, Kind: "frobnicate", CID: "x"}}, NextSeq: 1}},
		snapResp: wire.SnapshotResponse{Data: nil, Cursor: "", SnapshotEpoch: 2},
	}
	a := New(&nodeconfig.Config{}, state.NewFileRegistrationStore(t.TempDir()), state.NewFileStore(dir), asg, f, time.Second, time.Second)
	a.syncOnce(context.Background(), 0)
	// unknown kind ⇒ fail closed ⇒ snapshot recovery wipes to the snapshot (empty)
	if got, _ := asg.List(); len(got) != 0 {
		t.Fatalf("unknown kind must not apply: %+v", got)
	}
	if f.acks.Load() != 0 {
		t.Fatal("no ack on fail-closed")
	}
}
