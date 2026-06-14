// Package reload holds the live, hot-swappable effective config snapshot. It is
// separate from internal/config to avoid an import cycle (middleware, handlers,
// and storage all import internal/config and also need the live snapshot).
package reload

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/nova-archive/nova/internal/config"
)

// Store is the live source of effective config. A nil *Store is never used by
// callers; consumers nil-check the pointer and fall back to static values.
type Store struct {
	ptr     atomic.Pointer[config.Config]
	version atomic.Uint64
	mu      sync.Mutex // serializes writers (last-writer-wins / If-Match guard)
	overlay func(*config.Config)
	pinned  map[string]struct{}
	subs    []func(old, new *config.Config)
}

// New builds a Store. overlay (may be nil) re-applies env overrides onto a
// config so env keeps winning over yaml on every swap; pinned (may be nil) is
// the set of dotted-path keys that env overrides, surfaced by the admin API.
func New(initial *config.Config, overlay func(*config.Config), pinned map[string]struct{}) *Store {
	s := &Store{overlay: overlay, pinned: pinned}
	if overlay != nil {
		overlay(initial)
	}
	s.ptr.Store(initial)
	return s
}

// Load returns the current effective config (lock-free; never mutate it).
func (s *Store) Load() *config.Config { return s.ptr.Load() }

// Version is the monotonic snapshot version; the etag/If-Match source.
func (s *Store) Version() uint64 { return s.version.Load() }

// EnvPinned reports the env-overridden keys (for source metadata).
func (s *Store) EnvPinned() map[string]struct{} { return s.pinned }

// Subscribe registers a hot-reload callback. Call at wiring time only.
func (s *Store) Subscribe(fn func(old, new *config.Config)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

// Swap re-applies the env overlay onto next, stores it, bumps the version, and
// fires subscribers. Returns the new version. Subscriber panics are recovered
// so a misbehaving updater cannot leave the store half-applied.
func (s *Store) Swap(next *config.Config) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overlay != nil {
		s.overlay(next)
	}
	old := s.ptr.Load()
	s.ptr.Store(next)
	v := s.version.Add(1)
	for _, fn := range s.subs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("config reload subscriber panicked", "panic", r)
				}
			}()
			fn(old, next)
		}()
	}
	return v
}
