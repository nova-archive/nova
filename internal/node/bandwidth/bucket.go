// Package bandwidth implements the donor's AUTHORITATIVE budget enforcer (D11):
// the coordinator only reserves best-effort, but the node that actually sends
// bytes refuses work exceeding its configured budget. A classic token bucket
// with capacity == one day's budget and a per-second refill of budget/86400.
package bandwidth

import (
	"sync"
	"time"
)

// Bucket is a thread-safe token bucket. Tokens are bytes.
type Bucket struct {
	mu           sync.Mutex
	capacity     float64
	tokens       float64
	refillPerSec float64
	last         time.Time
}

// NewDailyBucket returns a bucket that allows bytesPerDay bytes per rolling day,
// starting full at now.
func NewDailyBucket(bytesPerDay int64, now time.Time) *Bucket {
	cap := float64(bytesPerDay)
	if cap < 0 { // defensive: a non-positive budget refuses all work
		cap = 0
	}
	return &Bucket{
		capacity:     cap,
		tokens:       cap,
		refillPerSec: cap / 86_400.0,
		last:         now,
	}
}

// Take attempts to consume n bytes as of now. A non-positive n is rejected
// (never credits tokens). It refills based on elapsed time (capped at
// capacity), then succeeds and deducts iff enough tokens remain. A request
// larger than capacity can never succeed.
func (b *Bucket) Take(n int64, now time.Time) bool {
	if n <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.refillPerSec
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if float64(n) > b.tokens {
		return false
	}
	b.tokens -= float64(n)
	return true
}
