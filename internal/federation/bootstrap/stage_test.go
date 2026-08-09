package bootstrap_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/nova-archive/nova/internal/federation/bootstrap"
)

func TestStage_ActivateMovesStagedContent(t *testing.T) {
	root := t.TempDir()
	s, err := bootstrap.NewStage(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("federation-ca.crt", []byte("CERT"), 0o644); err != nil {
		t.Fatal(err)
	}

	final := filepath.Join(root, "active", "federation-ca.crt")
	if err := s.Activate(map[string]string{"federation-ca.crt": final}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "CERT" {
		t.Fatalf("activated content = %q", b)
	}
}

func TestStage_ValidationFailureActivatesNothing(t *testing.T) {
	root := t.TempDir()
	s, _ := bootstrap.NewStage(root)
	if err := s.Put("bad.crt", []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := s.Validate(func(fs.FS) error { return errors.New("chain does not verify") })
	if err == nil {
		t.Fatal("want validation error")
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "active")); len(entries) != 0 {
		t.Fatal("a validation failure must leave the active federation untouched")
	}
}

func TestStage_PutHonoursPermissions(t *testing.T) {
	root := t.TempDir()
	s, _ := bootstrap.NewStage(root)
	if err := s.Put("secret.key", []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(s.Dir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("staged key mode = %o, want 600", mode)
	}
}

func TestDiscardOrphans_RemovesUnreferencedGenerations(t *testing.T) {
	root := t.TempDir()
	a, _ := bootstrap.NewStage(root)
	b, _ := bootstrap.NewStage(root)

	if err := bootstrap.DiscardOrphans(root, b.GenID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Dir()); !os.IsNotExist(err) {
		t.Fatal("an interrupted run's staging generation should be discarded")
	}
	if _, err := os.Stat(b.Dir()); err != nil {
		t.Fatal("the kept generation must survive")
	}
}

func TestWriteFileAtomic_ReplacesExistingContent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "operator.yaml")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.WriteFileAtomic(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "new" {
		t.Fatalf("content = %q", b)
	}
	info, _ := os.Stat(p)
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o, want 600", mode)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected only operator.yaml, got %d entries", len(entries))
	}
}
