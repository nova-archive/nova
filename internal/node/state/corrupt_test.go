package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P2-M7 corrupt-state drill (D-M7-4): what each donor state file does when its
// bytes are garbage, matching the failure-drills runbook's safe-to-delete
// table:
//   - registration.json: FAIL-FAST (an error, never ok=false) — silently
//     re-registering would mint a new identity and orphan the old node row.
//   - cursor.json: load error → the agent resyncs from zero, which the
//     coordinator answers with changes-from-0 or snapshot_required (safe to
//     delete; recovery is the M3 snapshot contract).
//   - progress.json: a cache of verified-pending acks — a corrupt file loads
//     EMPTY and the donor simply re-verifies (safe to delete).
func TestDrillCorruptStateFailsSafe(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// registration.json truncated to garbage → hard error naming the file.
	if err := os.WriteFile(filepath.Join(stateDir, "registration.json"), []byte("{\"node_id\": \"tru"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := NewFileRegistrationStore(dir).LoadRegistration(context.Background())
	if err == nil {
		t.Fatal("corrupt registration.json must fail-fast (never silently re-register)")
	}
	if ok {
		t.Fatal("corrupt registration must not read as a valid registration")
	}
	if !strings.Contains(err.Error(), "corrupt registration") {
		t.Fatalf("err = %v, want the corrupt-registration sentinel", err)
	}

	// cursor.json corrupted → load error (the agent treats it as cursor 0 and
	// re-enters the snapshot/replay recovery path).
	if err := os.WriteFile(filepath.Join(stateDir, "cursor.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(dir).Cursor(); err == nil {
		t.Fatal("corrupt cursor.json must surface a load error, not a fabricated cursor")
	}

	// progress.json corrupted → loads EMPTY and stays usable (re-verify path).
	if err := os.WriteFile(filepath.Join(stateDir, "progress.json"), []byte("###"), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, err := NewFileProgressStore(stateDir)
	if err != nil {
		t.Fatalf("corrupt progress.json must not brick the donor: %v", err)
	}
	if _, ok := ps.Get("any-cid"); ok {
		t.Fatal("corrupt progress must load empty, never partially-parsed")
	}
	if err := ps.Set("cid-1", Progress{AssignmentID: "a", Generation: 1, ByteSize: 10, State: ProgressVerifiedPending}); err != nil {
		t.Fatalf("store must be writable after corrupt load: %v", err)
	}
}
