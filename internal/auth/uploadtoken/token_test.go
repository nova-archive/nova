package uploadtoken_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/auth/uploadtoken"
	"github.com/stretchr/testify/require"
)

func TestGenerate(t *testing.T) {
	wire, id, hash, err := uploadtoken.Generate()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(wire, "nova_ut_"), "wire should have nova_ut_ prefix, got %q", wire)
	require.NotEqual(t, uuid.Nil, id, "id should not be nil UUID")
	require.NotEmpty(t, hash, "hash should not be empty")
}

func TestParseWireRoundTrip(t *testing.T) {
	wire, id, hash, err := uploadtoken.Generate()
	require.NoError(t, err)

	parsedID, secret, err := uploadtoken.ParseWire(wire)
	require.NoError(t, err)
	require.Equal(t, id, parsedID, "parsed id should match generated id")

	// HashSecret(secret) should match the hash from Generate()
	computedHash := uploadtoken.HashSecret(secret)
	require.Equal(t, hash, computedHash, "HashSecret(secret) should match stored hash")
}

func TestEqualHash(t *testing.T) {
	_, _, hash, err := uploadtoken.Generate()
	require.NoError(t, err)

	require.True(t, uploadtoken.EqualHash(hash, hash), "hash should equal itself")
	require.False(t, uploadtoken.EqualHash(hash, "0000000000000000000000000000000000000000000000000000000000000000"), "different hashes should not be equal")
}

func TestParseWireNotUploadToken(t *testing.T) {
	_, _, err := uploadtoken.ParseWire("eyJhbGciOiJFZERTQSJ9.someJWT.sig")
	require.ErrorIs(t, err, uploadtoken.ErrNotUploadToken, "JWT should return ErrNotUploadToken")
}

func TestParseWireMalformed(t *testing.T) {
	// Has the prefix but is malformed — not ErrNotUploadToken
	_, _, err := uploadtoken.ParseWire("nova_ut_NOTVALID")
	require.Error(t, err)
	require.NotErrorIs(t, err, uploadtoken.ErrNotUploadToken, "malformed nova_ut_ token should NOT return ErrNotUploadToken")
}

func TestParseWireFormat(t *testing.T) {
	wire, _, _, err := uploadtoken.Generate()
	require.NoError(t, err)

	// wire must have exactly one dot
	withoutPrefix := strings.TrimPrefix(wire, "nova_ut_")
	parts := strings.SplitN(withoutPrefix, ".", 2)
	require.Len(t, parts, 2, "wire format should have id.secret after prefix")
}
