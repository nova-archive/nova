package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileStore is the durable donor Store: the change-log cursor persists to
// <storageDir>/state/cursor.json (atomic temp→rename). The jti replay cache is
// in-memory for M3 (single-use repair tokens are M4).
type FileStore struct {
	dir  string
	mu   sync.Mutex
	jtis map[string]time.Time
}

func NewFileStore(storageDir string) *FileStore {
	return &FileStore{dir: filepath.Join(storageDir, "state"), jtis: map[string]time.Time{}}
}

func (f *FileStore) path() string { return filepath.Join(f.dir, "cursor.json") }

type cursorDoc struct {
	Seq int64 `json:"seq"`
}

func (f *FileStore) Cursor() (int64, error) {
	data, err := os.ReadFile(f.path())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var d cursorDoc
	if err := json.Unmarshal(data, &d); err != nil {
		return 0, err
	}
	return d.Seq, nil
}

func (f *FileStore) SetCursor(seq int64) error {
	data, _ := json.MarshalIndent(cursorDoc{Seq: seq}, "", "  ")
	return atomicWrite(f.dir, "cursor", data)
}

func (f *FileStore) SeenJTI(jti string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	exp, ok := f.jtis[jti]
	if !ok {
		return false, nil
	}
	if !time.Now().Before(exp) {
		delete(f.jtis, jti)
		return false, nil
	}
	return true, nil
}

func (f *FileStore) RecordJTI(jti string, exp time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jtis[jti] = exp
	return nil
}

var _ Store = (*FileStore)(nil)
