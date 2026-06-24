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

func TestMintReadGrantRoundTrip(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	s, err := tokens.NewSignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}

	const (
		donorID      = "d1234567-0000-0000-0000-000000000001"
		cid          = "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi"
		assignmentID = "a-read-grant"
	)
	now := time.Now()
	bootFloor := now.Add(-time.Minute)

	tok, err := s.MintReadGrant(donorID, cid, assignmentID, 1, 1<<20, 5*time.Minute, now, bootFloor)
	if err != nil {
		t.Fatalf("MintReadGrant: %v", err)
	}

	pub, err := wire.DecodePublicKey(s.PublicKeyWire())
	if err != nil {
		t.Fatal(err)
	}
	claims, err := wire.Verify(pub, tok, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if claims.SourceNodeID != donorID {
		t.Errorf("SourceNodeID = %q, want %q", claims.SourceNodeID, donorID)
	}
	if claims.DestNodeID != tokens.ReservedCoordinatorSourceID {
		t.Errorf("DestNodeID = %q, want %q", claims.DestNodeID, tokens.ReservedCoordinatorSourceID)
	}
	if claims.CID != cid {
		t.Errorf("CID = %q, want %q", claims.CID, cid)
	}
	if claims.AssignmentID != assignmentID {
		t.Errorf("AssignmentID = %q, want %q", claims.AssignmentID, assignmentID)
	}
	if claims.JTI == "" {
		t.Error("JTI must be non-empty")
	}
	if claims.ProtocolVersion != wire.ProtocolV1 {
		t.Errorf("ProtocolVersion = %q, want %q", claims.ProtocolVersion, wire.ProtocolV1)
	}
}
