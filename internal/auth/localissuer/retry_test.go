package localissuer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/auth/localissuer"
	"github.com/stretchr/testify/require"
)

func TestRetryUntil_SucceedsOnFirstAttempt(t *testing.T) {
	t.Parallel()
	calls := 0
	err := localissuer.RetryUntil(context.Background(), func() error {
		calls++
		return nil
	}, 3, time.Microsecond)
	require.NoError(t, err)
	require.Equal(t, 1, calls, "no retries needed when first attempt succeeds")
}

func TestRetryUntil_SucceedsOnRetry(t *testing.T) {
	t.Parallel()
	transient := errors.New("transient")
	calls := 0
	err := localissuer.RetryUntil(context.Background(), func() error {
		calls++
		if calls < 2 {
			return transient
		}
		return nil
	}, 3, time.Microsecond)
	require.NoError(t, err)
	require.Equal(t, 2, calls, "retried once and succeeded")
}

func TestRetryUntil_AllAttemptsFail(t *testing.T) {
	t.Parallel()
	persistent := errors.New("db down")
	calls := 0
	err := localissuer.RetryUntil(context.Background(), func() error {
		calls++
		return persistent
	}, 3, time.Microsecond)
	require.ErrorIs(t, err, persistent)
	require.Equal(t, 3, calls, "exactly maxAttempts calls when all fail")
}

func TestRetryUntil_RespectsContextCancellation(t *testing.T) {
	t.Parallel()
	transient := errors.New("transient")
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := localissuer.RetryUntil(ctx, func() error {
		calls++
		if calls == 1 {
			cancel() // ensure the wait between attempts returns ctx.Err
			return transient
		}
		return nil
	}, 5, 10*time.Second)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls, "no further attempts after cancellation")
}

// TestRetryUntil_MaxAttemptsOne_NoRetry confirms the boundary: a single
// attempt does exactly one op call and returns whatever it returned.
func TestRetryUntil_MaxAttemptsOne_NoRetry(t *testing.T) {
	t.Parallel()
	calls := 0
	err := localissuer.RetryUntil(context.Background(), func() error {
		calls++
		return errors.New("nope")
	}, 1, time.Second)
	require.Error(t, err)
	require.Equal(t, 1, calls)
}
