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

// replayClient always returns the same changes for since==0 (deterministic replay).
type replayClient struct{ resp wire.ChangesResponse }

func (replayClient) Register(context.Context, wire.RegisterRequest) (wire.RegisterResponse, error) {
	return wire.RegisterResponse{}, nil
}
func (replayClient) Heartbeat(context.Context, wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	return wire.HeartbeatResponse{}, nil
}
func (c replayClient) GetChanges(_ context.Context, since int64) (wire.ChangesResponse, error) {
	if since == 0 {
		return c.resp, nil
	}
	return wire.ChangesResponse{NextSeq: since, CurrentEpoch: since}, nil
}
func (replayClient) GetSnapshot(context.Context, string, int64) (wire.SnapshotResponse, error) {
	return wire.SnapshotResponse{}, nil
}

func TestSyncCrashBeforeCursorIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	c := replayClient{resp: wire.ChangesResponse{
		Changes: []wire.PinChange{{Sequence: 1, AssignmentID: "a", Generation: 1, Kind: "assign", CID: "bafy1", ByteSize: 5}},
		NextSeq: 1, CurrentEpoch: 1,
	}}
	a := New(&nodeconfig.Config{}, state.NewFileRegistrationStore(dir), state.NewFileStore(dir), state.NewFileAssignmentStore(dir), c, time.Hour, time.Hour)
	if next := a.syncOnce(context.Background(), 0); next != 1 {
		t.Fatalf("cursor=%d want 1", next)
	}
	// simulate crash BEFORE the cursor persisted: reset cursor to 0, keep the set
	if err := state.NewFileStore(dir).SetCursor(0); err != nil {
		t.Fatal(err)
	}
	// restart: reload stores, re-sync from 0 → the same change is replayed
	a2 := New(&nodeconfig.Config{}, state.NewFileRegistrationStore(dir), state.NewFileStore(dir), state.NewFileAssignmentStore(dir), c, time.Hour, time.Hour)
	a2.syncOnce(context.Background(), 0)
	got, _ := state.NewFileAssignmentStore(dir).List()
	if len(got) != 1 || got[0].Generation != 1 {
		t.Fatalf("re-apply not idempotent: %+v", got)
	}
}

// snap409Once returns ErrSnapshotEpochChanged on the first GetSnapshot, then resp.
type snap409Once struct {
	calls atomic.Int32
	resp  wire.SnapshotResponse
}

func (*snap409Once) Register(context.Context, wire.RegisterRequest) (wire.RegisterResponse, error) {
	return wire.RegisterResponse{}, nil
}
func (*snap409Once) Heartbeat(context.Context, wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	return wire.HeartbeatResponse{}, nil
}
func (*snap409Once) GetChanges(context.Context, int64) (wire.ChangesResponse, error) {
	return wire.ChangesResponse{}, ErrSnapshotRequired
}
func (c *snap409Once) GetSnapshot(context.Context, string, int64) (wire.SnapshotResponse, error) {
	if c.calls.Add(1) == 1 {
		return wire.SnapshotResponse{}, ErrSnapshotEpochChanged
	}
	return c.resp, nil
}

func TestRecoverSnapshotRestartsOn409(t *testing.T) {
	dir := t.TempDir()
	asg := state.NewFileAssignmentStore(dir)
	c := &snap409Once{resp: wire.SnapshotResponse{
		Data: []wire.SnapshotItem{{CID: "bafy1", AssignmentID: "a", Generation: 2, ByteSize: 5}}, Cursor: "", SnapshotEpoch: 7,
	}}
	a := New(&nodeconfig.Config{}, state.NewFileRegistrationStore(dir), state.NewFileStore(dir), asg, c, time.Hour, time.Hour)
	got := a.recoverSnapshot(context.Background(), 0)
	if got != 7 {
		t.Fatalf("cursor after 409-restart = %d, want 7", got)
	}
	if c.calls.Load() < 2 {
		t.Fatalf("GetSnapshot calls = %d, want >= 2 (restart)", c.calls.Load())
	}
	l, _ := asg.List()
	if len(l) != 1 || l[0].Generation != 2 {
		t.Fatalf("recovered set: %+v", l)
	}
}
