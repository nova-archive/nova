package jobs_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/jobs"
	"github.com/stretchr/testify/require"
)

func TestBackoffGrowsExponentiallyUntilCap(t *testing.T) {
	t.Parallel()
	// Attempts here are post-increment: attempts=1 means the first retry.
	tests := []struct {
		attempts int
		expect   time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{5, 80 * time.Second},
		{6, 160 * time.Second},
		{7, 300 * time.Second}, // cap
		{8, 300 * time.Second},
		{20, 300 * time.Second},
	}
	for _, tc := range tests {
		require.Equal(t, tc.expect, jobs.Backoff(tc.attempts), "attempts=%d", tc.attempts)
	}
}

func TestBackoffZeroAttemptsReturnsBase(t *testing.T) {
	t.Parallel()
	require.Equal(t, 5*time.Second, jobs.Backoff(0))
}
