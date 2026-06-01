package ratelimit_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/stretchr/testify/require"
)

func TestBucketAllowsBurstThenBlocks(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	l := ratelimit.NewLimiter(ratelimit.Config{RatePerSec: 10, Burst: 3}, clock)

	require.True(t, l.Allow("ip-a"))
	require.True(t, l.Allow("ip-a"))
	require.True(t, l.Allow("ip-a"))
	require.False(t, l.Allow("ip-a"), "burst of 3 exhausted")

	require.True(t, l.Allow("ip-b"), "separate key has its own bucket")

	now = now.Add(200 * time.Millisecond)
	require.True(t, l.Allow("ip-a"))
	require.True(t, l.Allow("ip-a"))
	require.False(t, l.Allow("ip-a"))
}

// TestLimiterEvictsOldestAtCap verifies that admitting a new key past the
// configured MaxKeys evicts the least-recently-accessed bucket.
func TestLimiterEvictsOldestAtCap(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	l := ratelimit.NewLimiter(ratelimit.Config{
		RatePerSec: 1, Burst: 5, MaxKeys: 3,
	}, clock)

	// Touch three distinct keys in order; each gets its own bucket.
	require.True(t, l.Allow("a"))
	now = now.Add(time.Second)
	require.True(t, l.Allow("b"))
	now = now.Add(time.Second)
	require.True(t, l.Allow("c"))
	require.Equal(t, 3, l.Len())

	// Re-touch "a" so it is now the most-recently accessed.
	now = now.Add(time.Second)
	require.True(t, l.Allow("a"))

	// Admit a fourth key — "b" is now the LRU (last touched at t=1s) and must
	// be the one evicted. Map stays at the cap.
	now = now.Add(time.Second)
	require.True(t, l.Allow("d"))
	require.Equal(t, 3, l.Len())

	// "b" should have been evicted; re-Allow gives it a fresh full burst.
	for i := 0; i < 5; i++ {
		require.True(t, l.Allow("b"), "fresh burst after eviction (i=%d)", i)
	}
	require.False(t, l.Allow("b"), "burst now exhausted")
}

// TestLimiterSweepRemovesStaleAndPreservesRecent verifies the periodic-sweep
// contract used by the coordinator gcLoop.
func TestLimiterSweepRemovesStaleAndPreservesRecent(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	l := ratelimit.NewLimiter(ratelimit.Config{
		RatePerSec: 1, Burst: 5, MaxKeys: 10,
	}, clock)

	// Insert two stale keys far in the past.
	require.True(t, l.Allow("stale-1"))
	require.True(t, l.Allow("stale-2"))

	// Advance time and insert two recent keys.
	now = now.Add(2 * time.Hour)
	require.True(t, l.Allow("recent-1"))
	require.True(t, l.Allow("recent-2"))
	require.Equal(t, 4, l.Len())

	// Sweep with a 1-hour window: the two stale keys are older than that and
	// must be evicted; the two recent ones must survive.
	evicted := l.Sweep(time.Hour)
	require.Equal(t, 2, evicted)
	require.Equal(t, 2, l.Len())

	// Re-Allow on a previously-stale key behaves as a fresh bucket.
	for i := 0; i < 5; i++ {
		require.True(t, l.Allow("stale-1"), "post-sweep fresh burst (i=%d)", i)
	}
	require.False(t, l.Allow("stale-1"))
}

// TestLimiterSweepZeroAge is a no-op (defends against misconfig).
func TestLimiterSweepZeroAge(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	l := ratelimit.NewLimiter(ratelimit.Config{
		RatePerSec: 1, Burst: 1,
	}, func() time.Time { return now })

	for i := 0; i < 5; i++ {
		require.True(t, l.Allow("k-"+strconv.Itoa(i)))
	}
	require.Equal(t, 5, l.Len())
	require.Equal(t, 0, l.Sweep(0))
	require.Equal(t, 0, l.Sweep(-time.Hour))
	require.Equal(t, 5, l.Len(), "no-op sweeps must not evict")
}
