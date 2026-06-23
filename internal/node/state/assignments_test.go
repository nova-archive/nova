package state

import "testing"

func TestAssignmentStoreApplyIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := NewFileAssignmentStore(dir)

	// assign gen 1
	if err := s.ApplyChanges([]ChangeInput{{AssignmentID: "a", Generation: 1, Kind: "assign", CID: "bafy1", ByteSize: 5}}); err != nil {
		t.Fatal(err)
	}
	// replay same change ⇒ no-op
	s.ApplyChanges([]ChangeInput{{AssignmentID: "a", Generation: 1, Kind: "assign", CID: "bafy1", ByteSize: 5}})
	// bump to gen 2
	s.ApplyChanges([]ChangeInput{{AssignmentID: "a", Generation: 2, Kind: "assign", CID: "bafy1", ByteSize: 5}})

	got, _ := NewFileAssignmentStore(dir).List() // reopened ⇒ durable
	if len(got) != 1 || got[0].Generation != 2 || got[0].State != "pending" {
		t.Fatalf("after assigns: %+v", got)
	}

	// stale generation is ignored
	s = NewFileAssignmentStore(dir)
	s.ApplyChanges([]ChangeInput{{AssignmentID: "a", Generation: 1, Kind: "unpin", CID: "bafy1"}})
	if got, _ := s.List(); len(got) != 1 {
		t.Fatalf("stale unpin must be ignored: %+v", got)
	}
	// current-generation unpin removes
	s.ApplyChanges([]ChangeInput{{AssignmentID: "a", Generation: 3, Kind: "unpin", CID: "bafy1"}})
	if got, _ := s.List(); len(got) != 0 {
		t.Fatalf("unpin should remove: %+v", got)
	}
}

func TestAssignmentStoreReplaceFromSnapshot(t *testing.T) {
	dir := t.TempDir()
	s := NewFileAssignmentStore(dir)
	s.ApplyChanges([]ChangeInput{{AssignmentID: "old", Generation: 1, Kind: "assign", CID: "stale", ByteSize: 1}})
	if err := s.Replace([]DesiredAssignment{{CID: "bafy1", AssignmentID: "a", Generation: 7, ByteSize: 5, State: "pending"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := NewFileAssignmentStore(dir).List()
	if len(got) != 1 || got[0].CID != "bafy1" || got[0].Generation != 7 {
		t.Fatalf("replace should wholesale-replace: %+v", got)
	}
}
