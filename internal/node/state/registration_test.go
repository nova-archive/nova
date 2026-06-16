package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileRegistrationRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewFileRegistrationStore(dir)
	ctx := context.Background()

	if _, ok, err := s.LoadRegistration(ctx); err != nil || ok {
		t.Fatalf("empty load: ok=%v err=%v", ok, err)
	}

	reg := Registration{NodeID: "n1", Fingerprint: "sha256:fp", SelectedProtocol: "fed/v1", RegisteredAt: time.Now().UTC().Truncate(time.Second)}
	if err := s.SaveRegistration(ctx, reg); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadRegistration(ctx)
	if err != nil || !ok {
		t.Fatalf("load after save: ok=%v err=%v", ok, err)
	}
	if got.NodeID != "n1" || got.Fingerprint != "sha256:fp" {
		t.Fatalf("got %+v", got)
	}

	info, err := os.Stat(filepath.Join(dir, "state", "registration.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "state"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}
