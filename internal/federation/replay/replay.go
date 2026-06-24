// Package replay provides a single-use JTI replay cache and boot-floor check
// for federation repair tokens. It is PURE STDLIB (sync + time only) so it can
// be imported by both the coordinator (internal/federation/coordinator) and the
// donor (cmd/node) without violating the donor dependency boundary.
package replay

import (
	"sync"
	"time"
)

// Cache is an in-memory single-use replay cache for repair-token jti values
// (D-M4-9). Entries expire at the token's not_after; combined with the
// boot-time floor, a restart leaves no usable replay window.
//
// The zero value is NOT valid; use New.
type Cache struct {
	mu   sync.Mutex
	seen map[string]time.Time // jti -> expiry
}

// New constructs a ready-to-use Cache.
func New() *Cache { return &Cache{seen: map[string]time.Time{}} }

// UseOnce returns true if jti has not been seen before (first use accepted),
// false if jti was already recorded (replay rejected). It opportunistically
// sweeps entries whose expiry is before now.
func (c *Cache) UseOnce(jti string, exp, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Sweep expired entries.
	for k, e := range c.seen {
		if now.After(e) {
			delete(c.seen, k)
		}
	}
	if _, ok := c.seen[jti]; ok {
		return false
	}
	c.seen[jti] = exp
	return true
}

// BootFloorOK returns true iff notBefore (Unix seconds) is at or after
// bootTime.Unix(). A token minted before the source last booted should be
// rejected regardless of signature validity.
func BootFloorOK(notBefore int64, bootTime time.Time) bool {
	return notBefore >= bootTime.Unix()
}
