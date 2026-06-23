package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Progress states (D-M4-5). desired/pending is the absence of a Progress entry.
const (
	ProgressVerifiedPending = "verified-ack-pending"
	ProgressAckDelivered    = "acked-delivered"
)

// Progress is the donor's local record of fetch/verify/ack for one CID.
type Progress struct {
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	ByteSize     int64  `json:"byte_size"`
	State        string `json:"state"`
}

// ProgressEntry pairs a CID with its progress (for PendingAcks).
type ProgressEntry struct {
	CID string
	Progress
}

// FileProgressStore persists Progress as a single atomic-JSON map under
// <stateDir>/progress.json (set-before-ack ordering lives in the agent).
type FileProgressStore struct {
	mu  sync.Mutex
	dir string
	m   map[string]Progress
}

// NewFileProgressStore loads <stateDir>/progress.json if present.
func NewFileProgressStore(stateDir string) (*FileProgressStore, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	s := &FileProgressStore{dir: stateDir, m: map[string]Progress{}}
	data, err := os.ReadFile(filepath.Join(stateDir, "progress.json"))
	if err == nil {
		_ = json.Unmarshal(data, &s.m)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// Get returns the Progress for cid, ok=false when absent.
func (s *FileProgressStore) Get(cid string) (Progress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[cid]
	return p, ok
}

// Set records progress for cid and flushes atomically to disk.
func (s *FileProgressStore) Set(cid string, p Progress) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[cid] = p
	return s.flushLocked()
}

// Clear removes the progress entry for cid and flushes atomically to disk.
func (s *FileProgressStore) Clear(cid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, cid)
	return s.flushLocked()
}

// PendingAcks returns all entries with State==ProgressVerifiedPending, sorted by CID.
func (s *FileProgressStore) PendingAcks() []ProgressEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ProgressEntry
	for cid, p := range s.m {
		if p.State == ProgressVerifiedPending {
			out = append(out, ProgressEntry{CID: cid, Progress: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CID < out[j].CID })
	return out
}

// flushLocked writes the map atomically (temp → fsync → rename → dir-fsync),
// reusing the package's atomicWrite helper from registration.go.
func (s *FileProgressStore) flushLocked() error {
	data, err := json.Marshal(s.m)
	if err != nil {
		return err
	}
	return atomicWrite(s.dir, "progress", data)
}
