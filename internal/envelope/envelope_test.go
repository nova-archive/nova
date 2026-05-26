package envelope_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

func TestDecodeRejectsTooShortBytes(t *testing.T) {
	t.Parallel()
	// Header is 32 bytes; anything less cannot be a valid envelope.
	for _, n := range []int{0, 1, 31} {
		buf := bytes.Repeat([]byte{0xAA}, n)
		_, _, err := envelope.Decode(buf)
		require.ErrorIs(t, err, envelope.ErrEnvelopeTooShort, "n=%d", n)
	}
}

func TestDecodeRejectsBadMagic(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 48)        // header + 16-byte tag minimum
	copy(buf[:4], []byte("XXXX"))  // not "NOVE"
	buf[4] = 0x01                  // version
	buf[5] = 0x01                  // algorithm
	_, _, err := envelope.Decode(buf)
	require.ErrorIs(t, err, envelope.ErrEnvelopeBadMagic)
}

func TestDecodeRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 48)
	copy(buf[:4], []byte("NOVE"))
	buf[4] = 0xFF                  // unknown version
	buf[5] = 0x01
	_, _, err := envelope.Decode(buf)
	require.ErrorIs(t, err, envelope.ErrEnvelopeUnsupported)
}

func TestDecodeRejectsNonZeroReserved(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 48)
	copy(buf[:4], []byte("NOVE"))
	buf[4] = 0x01
	buf[5] = 0x01
	buf[6] = 0x00
	buf[7] = 0x01                  // reserved must be 0x0000
	_, _, err := envelope.Decode(buf)
	require.ErrorIs(t, err, envelope.ErrEnvelopeUnsupported)
}

func TestDecodeRejectsBadAlgorithm(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 48)
	copy(buf[:4], []byte("NOVE"))
	buf[4] = 0x01
	buf[5] = 0x02                  // unknown algorithm
	_, _, err := envelope.Decode(buf)
	require.ErrorIs(t, err, envelope.ErrEnvelopeUnsupported)
}

func TestDecodeReturnsV1CodecOnValidHeader(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 48)
	copy(buf[:4], []byte("NOVE"))
	buf[4] = 0x01
	buf[5] = 0x01
	version, codec, err := envelope.Decode(buf)
	require.NoError(t, err)
	require.Equal(t, byte(0x01), version)
	require.NotNil(t, codec)
	require.Equal(t, byte(0x01), codec.Version())
}

func TestErrSentinelsAreDistinct(t *testing.T) {
	t.Parallel()
	require.False(t, errors.Is(envelope.ErrEnvelopeTooShort, envelope.ErrEnvelopeBadMagic))
	require.False(t, errors.Is(envelope.ErrEnvelopeBadMagic, envelope.ErrEnvelopeUnsupported))
}
