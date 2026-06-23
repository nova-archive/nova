package state_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/node/state"
)

func TestProgressPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	s, err := state.NewFileProgressStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("bafyX", state.Progress{AssignmentID: "a1", Generation: 1, ByteSize: 10, State: state.ProgressVerifiedPending}); err != nil {
		t.Fatal(err)
	}
	// reload from disk
	s2, _ := state.NewFileProgressStore(dir)
	p, ok := s2.Get("bafyX")
	if !ok || p.State != state.ProgressVerifiedPending || p.AssignmentID != "a1" {
		t.Fatalf("reload mismatch: %+v ok=%v", p, ok)
	}
	if got := s2.PendingAcks(); len(got) != 1 || got[0].CID != "bafyX" {
		t.Fatalf("pending acks: %+v", got)
	}
	if err := s2.Set("bafyX", state.Progress{AssignmentID: "a1", Generation: 1, State: state.ProgressAckDelivered}); err != nil {
		t.Fatal(err)
	}
	if got := s2.PendingAcks(); len(got) != 0 {
		t.Fatalf("expected no pending after delivered: %+v", got)
	}
}
