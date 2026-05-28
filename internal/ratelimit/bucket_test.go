package ratelimit_test

import (
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
