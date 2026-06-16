package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Registration is the donor's durable proof of a completed registration. It is
// the minimal state the agent needs to resume without re-registering.
type Registration struct {
	NodeID           string    `json:"node_id"`
	Fingerprint      string    `json:"fingerprint"`
	SelectedProtocol string    `json:"selected_protocol"`
	RegisteredAt     time.Time `json:"registered_at"`
}

// RegistrationStore persists the donor's registration. It is intentionally
// separate from the cursor/jti Store seam (M3/M4): different lifecycle, different
// durability needs.
type RegistrationStore interface {
	LoadRegistration(ctx context.Context) (Registration, bool, error)
	SaveRegistration(ctx context.Context, reg Registration) error
}

// FileRegistrationStore writes <storageDir>/state/registration.json atomically.
type FileRegistrationStore struct{ dir string }

// NewFileRegistrationStore roots the store under storageDir/state.
func NewFileRegistrationStore(storageDir string) *FileRegistrationStore {
	return &FileRegistrationStore{dir: filepath.Join(storageDir, "state")}
}

func (f *FileRegistrationStore) path() string { return filepath.Join(f.dir, "registration.json") }

// LoadRegistration returns the stored registration, ok=false when none exists.
func (f *FileRegistrationStore) LoadRegistration(_ context.Context) (Registration, bool, error) {
	data, err := os.ReadFile(f.path())
	if errors.Is(err, os.ErrNotExist) {
		return Registration{}, false, nil
	}
	if err != nil {
		return Registration{}, false, err
	}
	var reg Registration
	if err := json.Unmarshal(data, &reg); err != nil {
		return Registration{}, false, fmt.Errorf("state: corrupt registration.json: %w", err)
	}
	return reg, true, nil
}

// SaveRegistration writes atomically: temp file → fsync → rename → dir fsync.
func (f *FileRegistrationStore) SaveRegistration(_ context.Context, reg Registration) error {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.dir, "registration-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, f.path()); err != nil {
		return err
	}
	return fsyncDir(f.dir)
}

// fsyncDir flushes a directory entry so the rename survives a crash. Some
// filesystems reject directory fsync; that is tolerated.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

var _ RegistrationStore = (*FileRegistrationStore)(nil)
