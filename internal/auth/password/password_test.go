package password_test

import (
	"encoding/base64"
	"fmt"
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

// TestVerifyDegenerateNeverPanics asserts that Verify returns (false, error)
// and does NOT panic for stored hash strings that pass structural/Sscanf checks
// but carry degenerate argon2 parameters that would cause argon2.IDKey to panic.
func TestVerifyDegenerateNeverPanics(t *testing.T) {
	t.Parallel()

	b64 := base64.RawStdEncoding.EncodeToString
	salt := make([]byte, 16)   // zero salt is fine for this test
	digest := make([]byte, 32) // zero digest, valid length

	b64salt := b64(salt)
	b64digest := b64(digest)

	cases := []struct {
		name    string
		encoded string
	}{
		{
			name:    "empty digest (keyLen=0)",
			encoded: fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$", b64salt),
		},
		{
			name:    "t=0",
			encoded: fmt.Sprintf("$argon2id$v=19$m=65536,t=0,p=2$%s$%s", b64salt, b64digest),
		},
		{
			name:    "p=0",
			encoded: fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=0$%s$%s", b64salt, b64digest),
		},
		{
			name:    "m=0",
			encoded: fmt.Sprintf("$argon2id$v=19$m=0,t=3,p=2$%s$%s", b64salt, b64digest),
		},
		{
			name:    "empty salt",
			encoded: fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$$%s", b64digest),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ok bool
			var err error
			require.NotPanics(t, func() {
				ok, err = password.Verify(tc.encoded, "anything")
			})
			require.False(t, ok, "degenerate hash must not verify as matching")
			require.Error(t, err, "degenerate hash must return an error")
		})
	}
}
