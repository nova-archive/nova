// Package ratelimit provides a per-key token-bucket limiter used as
// in-process defense-in-depth. nginx is the primary limiter in production;
// this guards the coordinator directly.
package ratelimit

import (
	"sync"
	"time"
)

// Config tunes the limiter. RatePerSec is the steady refill; Burst is the
// bucket capacity.
type Config struct {
	RatePerSec float64
	Burst      float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a concurrency-safe keyed token-bucket limiter.
//
// M3 limitations (acceptable because nginx is the primary limiter and the
// coordinator is not directly internet-exposed in the Phase 1 topology):
//   - The per-key map is never evicted; distinct keys accumulate for the
//     process lifetime. TODO(post-M3): bound it (LRU or periodic sweep).
//   - Callers pass the client key; the middleware derives it from the
//     X-Forwarded-For header, which is only trustworthy behind a trusted
//     proxy. Do not rely on this limiter when the coordinator is reachable
//     directly. TODO(post-M3): honor XFF only from a trusted proxy set.
type Limiter struct {
	cfg  Config
	now  func() time.Time
	mu   sync.Mutex
	keys map[string]*bucket
}

// NewLimiter builds a limiter. clock may be nil (defaults to time.Now);
// tests inject a fixed clock.
func NewLimiter(cfg Config, clock func() time.Time) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	return &Limiter{cfg: cfg, now: clock, keys: make(map[string]*bucket)}
}

// Allow reports whether one event for key may proceed, consuming a token.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.keys[key]
	if !ok {
		b = &bucket{tokens: l.cfg.Burst, last: now}
		l.keys[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.cfg.RatePerSec
		if b.tokens > l.cfg.Burst {
			b.tokens = l.cfg.Burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
