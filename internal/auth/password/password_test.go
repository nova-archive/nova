package password_test

import (
	"sync"
	"testing"

	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/stretchr/testify/require"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	enc, err := password.Hash("correct horse battery staple")
	require.NoError(t, err)
	require.Contains(t, enc, "$argon2id$")

	ok, err := password.Verify(enc, "correct horse battery staple")
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = password.Verify(enc, "wrong")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	t.Parallel()
	_, err := password.Verify("not-a-hash", "x")
	require.Error(t, err)
}

func TestDummyVerifyNeverPanics(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() { password.DummyVerify("anything") })
}

func TestGateBoundsConcurrency(t *testing.T) {
	t.Parallel()
	g := password.NewGate(1)
	rel, ok := g.TryAcquire()
	require.True(t, ok)
	_, ok2 := g.TryAcquire()
	require.False(t, ok2, "second acquire over capacity must fail")
	rel()
	_, ok3 := g.TryAcquire()
	require.True(t, ok3, "release frees a slot")
}

func TestGateParallelSafe(t *testing.T) {
	t.Parallel()
	g := password.NewGate(4)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rel, ok := g.TryAcquire(); ok {
				rel()
			}
		}()
	}
	wg.Wait()
}
