package envelope_test

import (
	"crypto/rand"
	"testing"

	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

func TestWrapUnwrapRoundTrip(t *testing.T) {
	t.Parallel()
	mk := make([]byte, envelope.KeySize)
	pbk := make([]byte, envelope.KeySize)
	_, err := rand.Read(mk)
	require.NoError(t, err)
	_, err = rand.Read(pbk)
	require.NoError(t, err)

	wrapped, err := envelope.WrapKey(mk, pbk)
	require.NoError(t, err)
	require.Equal(t, envelope.WrappedKeySize, len(wrapped), "wrapped key MUST be 72 bytes")

	got, err := envelope.UnwrapKey(mk, wrapped)
	require.NoError(t, err)
	require.Equal(t, pbk, got)
}

func TestWrapDistinctNoncesPerCall(t *testing.T) {
	t.Parallel()
	mk := make([]byte, envelope.KeySize)
	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(mk)
	_, _ = rand.Read(pbk)

	a, err := envelope.WrapKey(mk, pbk)
	require.NoError(t, err)
	b, err := envelope.WrapKey(mk, pbk)
	require.NoError(t, err)

	require.NotEqual(t, a, b, "two wraps of the same key under the same master key MUST differ")
}

func TestWrapRejectsBadKeyLength(t *testing.T) {
	t.Parallel()
	good := make([]byte, envelope.KeySize)

	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := envelope.WrapKey(make([]byte, n), good)
		require.ErrorIs(t, err, envelope.ErrKeyWrongLength, "bad master key n=%d", n)
		_, err = envelope.WrapKey(good, make([]byte, n))
		require.ErrorIs(t, err, envelope.ErrKeyWrongLength, "bad per-blob key n=%d", n)
	}
}

func TestUnwrapRejectsBadWrappedLength(t *testing.T) {
	t.Parallel()
	mk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(mk)
	for _, n := range []int{0, 24, 48, 71, 73, 100} {
		_, err := envelope.UnwrapKey(mk, make([]byte, n))
		require.ErrorIs(t, err, envelope.ErrWrappedKeyWrongLength, "n=%d", n)
	}
}

func TestUnwrapRejectsWrongMasterKey(t *testing.T) {
	t.Parallel()
	mkA := make([]byte, envelope.KeySize)
	mkB := make([]byte, envelope.KeySize)
	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(mkA)
	_, _ = rand.Read(mkB)
	_, _ = rand.Read(pbk)

	wrapped, err := envelope.WrapKey(mkA, pbk)
	require.NoError(t, err)
	_, err = envelope.UnwrapKey(mkB, wrapped)
	require.ErrorIs(t, err, envelope.ErrEnvelopeAuthFailed)
}

func TestUnwrapRejectsTamperedWrapped(t *testing.T) {
	t.Parallel()
	mk := make([]byte, envelope.KeySize)
	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(mk)
	_, _ = rand.Read(pbk)

	wrapped, err := envelope.WrapKey(mk, pbk)
	require.NoError(t, err)

	tampered := append([]byte{}, wrapped...)
	tampered[envelope.NonceSize] ^= 0x01 // flip a ciphertext byte

	_, err = envelope.UnwrapKey(mk, tampered)
	require.ErrorIs(t, err, envelope.ErrEnvelopeAuthFailed)
}
