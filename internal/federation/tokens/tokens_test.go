package tokens_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func newClaims(now time.Time) wire.Claims {
	return wire.Claims{
		JTI: "jti-1", AssignmentID: "a-1", Generation: 1, CID: "bafy-x",
		SourceNodeID: tokens.ReservedCoordinatorSourceID, DestNodeID: "node-1",
		NotBefore: now.Unix(), NotAfter: now.Add(5 * time.Minute).Unix(),
		MaxBytes: 1 << 20, ProtocolVersion: wire.ProtocolV1,
	}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	s, err := tokens.NewSignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, err := s.Mint(newClaims(now))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := wire.DecodePublicKey(s.PublicKeyWire())
	if err != nil {
		t.Fatal(err)
	}
	got, err := wire.Verify(pub, tok, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.AssignmentID != "a-1" || got.SourceNodeID != tokens.ReservedCoordinatorSourceID {
		t.Fatalf("claims mismatch: %+v", got)
	}
}

func TestMintRejectsBadSeed(t *testing.T) {
	if _, err := tokens.NewSignerFromSeed([]byte("short")); err == nil {
		t.Fatal("expected seed-length error")
	}
}

func TestVerifyRejectsTamperAndExpiry(t *testing.T) {
	seed := make([]byte, 32)
	s, _ := tokens.NewSignerFromSeed(seed)
	now := time.Now()
	tok, _ := s.Mint(newClaims(now))
	pub, _ := wire.DecodePublicKey(s.PublicKeyWire())
	if _, err := wire.Verify(pub, tok+"x", now); err == nil {
		t.Fatal("expected signature error on tamper")
	}
	if _, err := wire.Verify(pub, tok, now.Add(10*time.Minute)); err == nil {
		t.Fatal("expected expiry error")
	}
}
