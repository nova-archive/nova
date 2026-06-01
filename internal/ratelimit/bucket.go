// Package ratelimit provides a per-key token-bucket limiter used as
// in-process defense-in-depth. nginx is the primary limiter in production;
// this guards the coordinator directly.
package ratelimit

import (
	"sync"
	"time"
)

// defaultMaxKeys bounds the number of distinct keys held concurrently when
// Config.MaxKeys is zero. 100k buckets at ~48 bytes each is a ~5 MB upper
// bound on per-process limiter state, comfortable for any realistic dev or
// single-operator production deployment.
const defaultMaxKeys = 100_000

// Config tunes the limiter. RatePerSec is the steady refill; Burst is the
// bucket capacity. MaxKeys bounds the number of distinct keys held
// concurrently (defaults to 100_000); when the cap is reached, the
// least-recently-accessed key is evicted before a new one is admitted.
type Config struct {
	RatePerSec float64
	Burst      float64
	MaxKeys    int
}

// bucket holds per-key token state. `last` is both the refill anchor and
// the LRU eviction anchor — Allow updates it on every call (regardless of
// allow/deny outcome), so the field accurately tracks last-access time.
type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a concurrency-safe keyed token-bucket limiter.
//
// M6.2 hardening:
//   - The per-key map is bounded by Config.MaxKeys (default 100_000). When
//     the cap is reached, the least-recently-accessed bucket is evicted
//     before a new one is admitted. Eviction is O(N) over the map but only
//     fires when the cap is hit; periodic Sweep keeps the cap from being
//     hit in normal traffic.
//   - Callers should invoke Sweep periodically (the coordinator's gcLoop
//     does this with a 1 h maxAge) to drop buckets that have not been
//     accessed for the configured window.
//   - Callers pass the client key; the middleware derives it from the
//     X-Forwarded-For header. XFF is only trustworthy behind a trusted
//     proxy; M6.2 B2 adds explicit NOVA_TRUSTED_PROXIES enforcement so
//     direct-exposure deployments are safe-by-default.
type Limiter struct {
	cfg     Config
	maxKeys int
	now     func() time.Time
	mu      sync.Mutex
	keys    map[string]*bucket
}

// NewLimiter builds a limiter. clock may be nil (defaults to time.Now);
// tests inject a fixed clock.
func NewLimiter(cfg Config, clock func() time.Time) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	max := cfg.MaxKeys
	if max <= 0 {
		max = defaultMaxKeys
	}
	return &Limiter{cfg: cfg, maxKeys: max, now: clock, keys: make(map[string]*bucket)}
}

// Allow reports whether one event for key may proceed, consuming a token.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.keys[key]
	if !ok {
		// Cap enforcement: evict the LRU entry before admitting a new one.
		if len(l.keys) >= l.maxKeys {
			l.evictLRULocked()
		}
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

// Sweep deletes buckets whose last-access is older than maxAge. Intended
// to be called periodically (typically alongside the coordinator's GC
// tick) to keep the limiter's memory footprint stable in production.
// Returns the number of evicted entries. maxAge <= 0 is a no-op.
func (l *Limiter) Sweep(maxAge time.Duration) int {
	if maxAge <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-maxAge)
	evicted := 0
	for k, b := range l.keys {
		if b.last.Before(cutoff) {
			delete(l.keys, k)
			evicted++
		}
	}
	return evicted
}

// Len returns the current number of tracked keys (test/observability).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

// evictLRULocked removes the single least-recently-accessed bucket from
// the map. Caller MUST hold l.mu.
func (l *Limiter) evictLRULocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, b := range l.keys {
		if first || b.last.Before(oldestTime) {
			oldestKey = k
			oldestTime = b.last
			first = false
		}
	}
	if !first {
		delete(l.keys, oldestKey)
	}
}
