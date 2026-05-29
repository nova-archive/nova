package upload

import (
	"sync"

	"github.com/google/uuid"
)

// lockmap hands out one mutex per session id so concurrent PATCHes on the same
// session serialize within a single coordinator (the tusd MemoryLocker model).
// Entries are removed on finalize/abort/GC; a small residual set for in-flight
// uploads is acceptable for Phase 1's bounded session volume.
type lockmap struct {
	mu sync.Mutex
	m  map[uuid.UUID]*sync.Mutex
}

func newLockmap() *lockmap { return &lockmap{m: make(map[uuid.UUID]*sync.Mutex)} }

func (l *lockmap) get(id uuid.UUID) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m[id] == nil {
		l.m[id] = &sync.Mutex{}
	}
	return l.m[id]
}

func (l *lockmap) forget(id uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, id)
}
