package envelope_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return buf
}

func TestV1RoundTripEmpty(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	v1 := envelope.V1()

	env, err := v1.Encrypt(nil, key)
	require.NoError(t, err)

	got, err := v1.Decrypt(env, key)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestV1RoundTripSmall(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	plain := []byte("hello, nova")
	v1 := envelope.V1()

	env, err := v1.Encrypt(plain, key)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(env, []byte("NOVE")), "envelope must start with NOVE magic")
	require.Equal(t, envelope.HeaderSize+len(plain)+envelope.TagSize, len(env))

	got, err := v1.Decrypt(env, key)
	require.NoError(t, err)
	require.Equal(t, plain, got)
}

func TestV1RoundTripLarge(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	plain := randBytes(t, 5*1024*1024) // 5 MiB
	v1 := envelope.V1()

	env, err := v1.Encrypt(plain, key)
	require.NoError(t, err)
	got, err := v1.Decrypt(env, key)
	require.NoError(t, err)
	require.Equal(t, plain, got)
}

func TestV1DistinctNoncesPerEncrypt(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	plain := []byte("identical bytes")
	v1 := envelope.V1()

	a, err := v1.Encrypt(plain, key)
	require.NoError(t, err)
	b, err := v1.Encrypt(plain, key)
	require.NoError(t, err)

	require.NotEqual(t, a, b,
		"two encryptions of identical plaintext under identical key must differ "+
			"(would otherwise leak equality information via CID collision)")
	// Nonces sit at offset 8..32 in the envelope.
	require.NotEqual(t, a[8:32], b[8:32], "nonces must differ")
}

func TestV1EncryptRejectsWrongKeyLength(t *testing.T) {
	t.Parallel()
	v1 := envelope.V1()
	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := v1.Encrypt([]byte("hi"), make([]byte, n))
		require.ErrorIs(t, err, envelope.ErrKeyWrongLength, "n=%d", n)
	}
}

func TestV1DecryptRejectsWrongKeyLength(t *testing.T) {
	t.Parallel()
	v1 := envelope.V1()
	key := randBytes(t, envelope.KeySize)
	env, err := v1.Encrypt([]byte("hi"), key)
	require.NoError(t, err)
	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := v1.Decrypt(env, make([]byte, n))
		require.ErrorIs(t, err, envelope.ErrKeyWrongLength, "n=%d", n)
	}
}

func TestV1DecryptDetectsTampering(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	plain := []byte("must not survive tampering")
	v1 := envelope.V1()

	env, err := v1.Encrypt(plain, key)
	require.NoError(t, err)

	// Flip a ciphertext byte (after the header, before the tag).
	tampered := append([]byte{}, env...)
	tampered[envelope.HeaderSize] ^= 0x01

	_, err = v1.Decrypt(tampered, key)
	require.ErrorIs(t, err, envelope.ErrEnvelopeAuthFailed)
}

func TestV1DecryptDetectsTagFlip(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	plain := []byte("must not survive tampering")
	v1 := envelope.V1()

	env, err := v1.Encrypt(plain, key)
	require.NoError(t, err)

	tampered := append([]byte{}, env...)
	tampered[len(tampered)-1] ^= 0x01

	_, err = v1.Decrypt(tampered, key)
	require.ErrorIs(t, err, envelope.ErrEnvelopeAuthFailed)
}

func TestV1DecryptRejectsWrongKey(t *testing.T) {
	t.Parallel()
	keyA := randBytes(t, envelope.KeySize)
	keyB := randBytes(t, envelope.KeySize)
	v1 := envelope.V1()

	env, err := v1.Encrypt([]byte("payload"), keyA)
	require.NoError(t, err)
	_, err = v1.Decrypt(env, keyB)
	require.ErrorIs(t, err, envelope.ErrEnvelopeAuthFailed)
}

func TestV1HeaderShape(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	v1 := envelope.V1()

	env, err := v1.Encrypt([]byte("x"), key)
	require.NoError(t, err)

	require.Equal(t, byte('N'), env[0])
	require.Equal(t, byte('O'), env[1])
	require.Equal(t, byte('V'), env[2])
	require.Equal(t, byte('E'), env[3])
	require.Equal(t, envelope.VersionV1, env[4])
	require.Equal(t, envelope.AlgorithmXChaCha20Poly1305, env[5])
	require.Equal(t, byte(0), env[6])
	require.Equal(t, byte(0), env[7])
}
