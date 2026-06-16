package bandwidth_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/node/bandwidth"
	"github.com/stretchr/testify/require"
)

func TestBucketAllowsUpToBudgetThenRefuses(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := bandwidth.NewDailyBucket(1000, now)
	require.True(t, b.Take(600, now))
	require.True(t, b.Take(400, now)) // exactly the budget consumed
	require.False(t, b.Take(1, now))  // over budget — refused
}

func TestBucketRefillsOverTime(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := bandwidth.NewDailyBucket(86_400, now) // 1 byte/sec
	require.True(t, b.Take(86_400, now))       // drain
	require.False(t, b.Take(1, now))
	later := now.Add(10 * time.Second)
	require.True(t, b.Take(10, later)) // 10s × 1 B/s refilled
	require.False(t, b.Take(1, later))
}

func TestBucketRefusesSingleRequestExceedingCapacity(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := bandwidth.NewDailyBucket(1000, now)
	require.False(t, b.Take(1001, now)) // larger than a full day's budget
}

func TestBucketRefillCapsAtCapacity(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := bandwidth.NewDailyBucket(1000, now)
	require.True(t, b.Take(1000, now))
	veryLater := now.Add(48 * time.Hour) // would over-refill if uncapped
	require.True(t, b.Take(1000, veryLater))
	require.False(t, b.Take(1, veryLater)) // capacity, not 2×
}

func TestBucketRejectsNonPositiveTake(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := bandwidth.NewDailyBucket(1000, now)
	require.False(t, b.Take(0, now))
	require.False(t, b.Take(-100, now)) // must NOT credit tokens
	require.True(t, b.Take(1000, now))  // budget intact after the bad takes
}

func TestBucketWithNonPositiveBudgetRefusesAll(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	require.False(t, bandwidth.NewDailyBucket(0, now).Take(1, now))
	require.False(t, bandwidth.NewDailyBucket(-5, now).Take(1, now))
}
