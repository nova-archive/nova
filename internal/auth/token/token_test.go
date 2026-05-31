package token_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/auth/token"
	"github.com/stretchr/testify/require"
)

func newSigner(t *testing.T) *token.Signer {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	_, err := rand.Read(seed)
	require.NoError(t, err)
	s, err := token.NewSignerFromSeed(hex.EncodeToString(seed))
	require.NoError(t, err)
	return s
}

func TestSignVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	s := newSigner(t)
	v := token.NewVerifier(s.KID(), s.Public())
	raw, err := s.Sign(token.Mint{Subject: "u1", Role: "operator", Issuer: "https://nova/", Audience: "nova", TTL: time.Minute})
	require.NoError(t, err)

	claims, err := v.Verify(raw)
	require.NoError(t, err)
	require.Equal(t, "u1", claims.Subject)
	require.Equal(t, "operator", claims.Role)
	require.Equal(t, "https://nova/", claims.Issuer)
}

func TestVerifyRejectsExpired(t *testing.T) {
	t.Parallel()
	s := newSigner(t)
	v := token.NewVerifier(s.KID(), s.Public())
	raw, err := s.Sign(token.Mint{Subject: "u1", Role: "viewer", Issuer: "https://nova/", Audience: "nova", TTL: -time.Minute})
	require.NoError(t, err)
	_, err = v.Verify(raw)
	require.Error(t, err)
}

func TestVerifyRejectsUnknownKID(t *testing.T) {
	t.Parallel()
	s1, s2 := newSigner(t), newSigner(t)
	v := token.NewVerifier(s1.KID(), s1.Public()) // only knows s1
	raw, err := s2.Sign(token.Mint{Subject: "u1", Role: "viewer", Issuer: "https://nova/", Audience: "nova", TTL: time.Minute})
	require.NoError(t, err)
	_, err = v.Verify(raw)
	require.Error(t, err)
}

func TestJWKSContainsPublicKey(t *testing.T) {
	t.Parallel()
	s := newSigner(t)
	jwks, err := s.JWKS()
	require.NoError(t, err)
	require.Contains(t, string(jwks), s.KID())
	require.Contains(t, string(jwks), `"OKP"`)
	require.Contains(t, string(jwks), `"Ed25519"`)
}
