package state

import (
	"path/filepath"
	"testing"
)

func TestFileStoreCursorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	if c, err := s.Cursor(); err != nil || c != 0 {
		t.Fatalf("empty cursor: c=%d err=%v", c, err)
	}
	if err := s.SetCursor(4172836); err != nil {
		t.Fatal(err)
	}
	// reopen ⇒ survives
	if c, err := NewFileStore(dir).Cursor(); err != nil || c != 4172836 {
		t.Fatalf("reopened cursor: c=%d err=%v", c, err)
	}
	if _, err := filepath.Glob(filepath.Join(dir, "state", "*.tmp")); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreImplementsStore(t *testing.T) {
	var _ Store = NewFileStore(t.TempDir())
}
