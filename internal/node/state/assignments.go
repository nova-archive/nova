package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// DesiredAssignment is one entry of the donor's local desired-assignment set —
// coordinator INTENT this node hold a CID. It is NOT evidence the bytes are held
// (that is an ack, M4). State is "pending" until M4's fetch→verify→ack.
type DesiredAssignment struct {
	CID          string `json:"cid"`
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	ByteSize     int64  `json:"byte_size"`
	State        string `json:"state"` // "pending" in M3
}

// ChangeInput is a primitive view of a wire.PinChange (state stays wire-free).
type ChangeInput struct {
	AssignmentID string
	Generation   int64
	Kind         string // "assign" | "unpin"
	CID          string
	ByteSize     int64
}

// AssignmentStore is the donor's durable desired-assignment set.
type AssignmentStore interface {
	ApplyChanges(changes []ChangeInput) error // idempotent by (assignment_id, generation)
	Replace(items []DesiredAssignment) error  // wholesale snapshot replace
	List() ([]DesiredAssignment, error)
}

// FileAssignmentStore persists the set to <storageDir>/state/assignments.json.
type FileAssignmentStore struct {
	dir string
	mu  sync.Mutex
}

func NewFileAssignmentStore(storageDir string) *FileAssignmentStore {
	return &FileAssignmentStore{dir: filepath.Join(storageDir, "state")}
}

func (f *FileAssignmentStore) path() string { return filepath.Join(f.dir, "assignments.json") }

func (f *FileAssignmentStore) load() (map[string]DesiredAssignment, error) {
	data, err := os.ReadFile(f.path())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]DesiredAssignment{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]DesiredAssignment
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *FileAssignmentStore) persist(m map[string]DesiredAssignment) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	return atomicWrite(f.dir, "assignments", data)
}

func (f *FileAssignmentStore) ApplyChanges(changes []ChangeInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	for _, c := range changes {
		cur, ok := m[c.CID]
		// idempotent: only act on a generation >= what we have for this assignment.
		if ok && cur.AssignmentID == c.AssignmentID && c.Generation < cur.Generation {
			continue
		}
		switch c.Kind {
		case "assign":
			m[c.CID] = DesiredAssignment{CID: c.CID, AssignmentID: c.AssignmentID, Generation: c.Generation, ByteSize: c.ByteSize, State: "pending"}
		case "unpin":
			delete(m, c.CID)
		}
	}
	return f.persist(m)
}

func (f *FileAssignmentStore) Replace(items []DesiredAssignment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := make(map[string]DesiredAssignment, len(items))
	for _, it := range items {
		if it.State == "" {
			it.State = "pending"
		}
		m[it.CID] = it
	}
	return f.persist(m)
}

func (f *FileAssignmentStore) List() ([]DesiredAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return nil, err
	}
	out := make([]DesiredAssignment, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CID < out[j].CID })
	return out, nil
}

var _ AssignmentStore = (*FileAssignmentStore)(nil)
