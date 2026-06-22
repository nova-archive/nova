package wire

import (
	"encoding/json"
	"testing"
)

func TestPinChangeTags(t *testing.T) {
	b, _ := json.Marshal(PinChange{Sequence: 7, AssignmentID: "a", Generation: 2, Kind: "assign", CID: "bafy", ByteSize: 1048576})
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"seq", "assignment_id", "generation", "kind", "cid", "byte_size"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("PinChange missing %q in %s", k, b)
		}
	}
	if _, ok := m["source"]; ok {
		t.Fatalf("source must be omitted when nil: %s", b)
	}
}

func TestChangesResponseTags(t *testing.T) {
	b, _ := json.Marshal(ChangesResponse{Changes: []PinChange{}, NextSeq: 9, CurrentEpoch: 12})
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)
	for _, k := range []string{"changes", "next_seq", "current_epoch"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("ChangesResponse missing %q in %s", k, b)
		}
	}
}

func TestSnapshotResponseRoundTrip(t *testing.T) {
	in := SnapshotResponse{
		Data:          []SnapshotItem{{CID: "bafy", AssignmentID: "a", Generation: 1, ByteSize: 5}},
		Cursor:        "bafy",
		SnapshotEpoch: 421,
	}
	b, _ := json.Marshal(in)
	var out SnapshotResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.SnapshotEpoch != 421 || len(out.Data) != 1 || out.Data[0].AssignmentID != "a" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestAckFailExtraFieldsAndCode(t *testing.T) {
	b, _ := json.Marshal(Ack{AssignmentID: "a", Generation: 1, CID: "bafy", ByteSize: 5, IPFSPinStatus: "pinned", FetchedFromNodeID: "n"})
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)
	for _, k := range []string{"byte_size", "ipfs_pin_status", "fetched_from_node_id"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("Ack missing %q", k)
		}
	}
	if CodeStaleAssignment == "" {
		t.Fatal("CodeStaleAssignment must be defined")
	}
	if FailReasonOutOfSpace == "" {
		t.Fatal("Fail reason constants must be defined")
	}
	if NormalizeFailReason("") != FailReasonOther || NormalizeFailReason("bogus") != "" {
		t.Fatal(`NormalizeFailReason: ""→other, unknown→""`)
	}
}
