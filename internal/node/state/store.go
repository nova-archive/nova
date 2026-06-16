// Package state is the donor's LOCAL persistence seam — assignment cursor, cert
// material handles, and the single-use repair-token jti replay cache. It uses
// NO Postgres (donors hold no catalog). M1 ships the interface + an in-memory
// stub; the durable file/KV implementation lands in M3/M4.
package state

import "time"

// Store is the donor's local state. Cursor methods back the change-log sync
// (M3); the jti methods back single-use repair-token enforcement (M4).
type Store interface {
	Cursor() (int64, error)
	SetCursor(seq int64) error
	SeenJTI(jti string) (bool, error)
	RecordJTI(jti string, exp time.Time) error
}

// MemStore is an in-memory Store stub for M1/tests.
type MemStore struct {
	cursor int64
	jtis   map[string]time.Time
}

func NewMemStore() *MemStore { return &MemStore{jtis: map[string]time.Time{}} }

func (m *MemStore) Cursor() (int64, error)    { return m.cursor, nil }
func (m *MemStore) SetCursor(seq int64) error { m.cursor = seq; return nil }

// SeenJTI reports whether jti was recorded and has NOT expired. Expired entries
// are pruned lazily so the replay cache cannot report a stale hit or grow
// unbounded — the semantics M4's source-side single-use enforcement relies on.
func (m *MemStore) SeenJTI(jti string) (bool, error) {
	exp, ok := m.jtis[jti]
	if !ok {
		return false, nil
	}
	if !time.Now().Before(exp) {
		delete(m.jtis, jti)
		return false, nil
	}
	return true, nil
}

func (m *MemStore) RecordJTI(jti string, exp time.Time) error {
	m.jtis[jti] = exp
	return nil
}

var _ Store = (*MemStore)(nil)
