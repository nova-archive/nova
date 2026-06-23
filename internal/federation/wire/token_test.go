package wire_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/stretchr/testify/require"
)

func sampleClaims(now time.Time) wire.Claims {
	return wire.Claims{
		JTI: "jti-1", AssignmentID: "a-1", Generation: 7, CID: "bafyTEST",
		SourceNodeID: "src", DestNodeID: "dst",
		NotBefore: now.Add(-time.Minute).Unix(), NotAfter: now.Add(time.Minute).Unix(),
		MaxBytes: 1 << 20, ProtocolVersion: wire.ProtocolV1,
	}
}

// signTestToken builds a valid token via the exported format primitives. There
// is intentionally NO wire.SignToken (minting with a private key is
// coordinator-only, M4); tests sign locally with a throwaway key.
func signTestToken(t *testing.T, priv ed25519.PrivateKey, c wire.Claims) string {
	t.Helper()
	si, err := wire.SigningInput(c)
	require.NoError(t, err)
	return wire.AssembleToken(si, ed25519.Sign(priv, []byte(si)))
}

func TestTokenRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	tok := signTestToken(t, priv, sampleClaims(now))
	require.Contains(t, tok, ".")
	got, err := wire.Verify(pub, tok, now)
	require.NoError(t, err)
	require.Equal(t, "jti-1", got.JTI)
	require.Equal(t, int64(7), got.Generation)
}

func TestTokenExpired(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	tok := signTestToken(t, priv, sampleClaims(now))
	_, err := wire.Verify(pub, tok, now.Add(2*time.Minute))
	require.ErrorIs(t, err, wire.ErrExpired)
}

func TestTokenNotYetValid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	tok := signTestToken(t, priv, sampleClaims(now))
	_, err := wire.Verify(pub, tok, now.Add(-2*time.Minute))
	require.ErrorIs(t, err, wire.ErrNotYetValid)
}

func TestTokenWrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	tok := signTestToken(t, priv, sampleClaims(now))
	_, err := wire.Verify(otherPub, tok, now)
	require.ErrorIs(t, err, wire.ErrBadSignature)
}

func TestTokenTamperedClaim(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	tok := signTestToken(t, priv, sampleClaims(now))
	seg, sig, _ := strings.Cut(tok, ".")
	// Flip a byte in the middle of the claims segment to a guaranteed-different
	// base64url char. The segment still decodes, but the signature (made over the
	// ORIGINAL segment) no longer matches → ErrBadSignature.
	b := []byte(seg)
	i := len(b) / 2
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	_, err := wire.Verify(pub, string(b)+"."+sig, now)
	require.ErrorIs(t, err, wire.ErrBadSignature)
}

func TestTokenMissingJTI(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	c := sampleClaims(now)
	c.JTI = ""
	tok := signTestToken(t, priv, c)
	_, err := wire.Verify(pub, tok, now)
	require.ErrorIs(t, err, wire.ErrMalformedClaims)
}

func TestTokenRejectsWrongProtocol(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	c := sampleClaims(now)
	c.ProtocolVersion = "fed/v99"
	tok := signTestToken(t, priv, c)
	_, err := wire.Verify(pub, tok, now)
	require.ErrorIs(t, err, wire.ErrMalformedClaims)
}

func TestTokenRejectsNonPositiveGenerationAndBytes(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	for _, mut := range []func(c *wire.Claims){
		func(c *wire.Claims) { c.Generation = 0 },
		func(c *wire.Claims) { c.MaxBytes = 0 },
		func(c *wire.Claims) { c.NotBefore, c.NotAfter = c.NotAfter, c.NotBefore },
	} {
		c := sampleClaims(now)
		mut(&c)
		_, err := wire.Verify(pub, signTestToken(t, priv, c), now)
		require.ErrorIs(t, err, wire.ErrMalformedClaims)
	}
}

func TestTokenMalformedToken(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	_, err := wire.Verify(pub, "not-a-token", time.Now())
	require.ErrorIs(t, err, wire.ErrMalformedToken)
}

func TestPublicKeyRoundTrip(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	s := wire.EncodePublicKey(pub)
	got, err := wire.DecodePublicKey(s)
	if err != nil || !got.Equal(pub) {
		t.Fatalf("round-trip failed: %v", err)
	}
	if _, err := wire.DecodePublicKey("not-base64-!!"); err == nil {
		t.Fatal("expected decode error")
	}
	if _, err := wire.DecodePublicKey(wire.EncodePublicKey(pub)[:10]); err == nil {
		t.Fatal("expected length error")
	}
}
