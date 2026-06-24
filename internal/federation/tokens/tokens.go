// Package tokens is the coordinator-ONLY Ed25519 repair-token mint (D1). It holds
// the private signing key; donors only ever Verify (internal/federation/wire).
// This package MUST NEVER enter the cmd/node build graph.
package tokens

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/secret"
)

// ReservedCoordinatorSourceID aliases the shared protocol constant
// wire.CoordinatorSourceID (D-M4-2) so coordinator-side code reads naturally; the
// donor references wire.CoordinatorSourceID directly (it cannot import this
// operator-only package). It is NOT a nodes row; donors echo it as
// Ack.FetchedFromNodeID.
const ReservedCoordinatorSourceID = wire.CoordinatorSourceID

// Secret resolver coordinates for the repair-signing seed (base64url or hex,
// 32-byte Ed25519 seed). Keys never enter the DB (D-M4-7).
const (
	envKey           = "NOVA_FEDERATION_REPAIR_SIGNING_KEY"
	envFileKey       = "NOVA_FEDERATION_REPAIR_SIGNING_KEY_FILE"
	defaultMountPath = "/run/secrets/federation-repair-signing-key"
)

// Signer mints repair tokens. Construct via LoadSigner or NewSignerFromSeed.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// LoadSigner resolves the seed via the standard secret chain (env / _FILE /
// mount path) and derives the key. defaultPath is the operator-configured
// federation.repair_signing_key_path; empty selects the built-in mount default.
func LoadSigner(defaultPath string) (*Signer, error) {
	mountPath := defaultPath
	if mountPath == "" {
		mountPath = defaultMountPath
	}
	v, _, err := secret.ResolveSecret(envKey, envFileKey, mountPath)
	if err != nil {
		return nil, fmt.Errorf("tokens: load repair-signing key: %w", err)
	}
	seed, err := decodeSeed(strings.TrimSpace(v))
	if err != nil {
		return nil, err
	}
	return NewSignerFromSeed(seed)
}

func decodeSeed(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(b) == ed25519.SeedSize {
		return b, nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == ed25519.SeedSize {
		return b, nil
	}
	return nil, errors.New("tokens: seed must be base64url or hex of 32 bytes")
}

// NewSignerFromSeed derives an Ed25519 keypair from a 32-byte seed.
func NewSignerFromSeed(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("tokens: seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, nil
}

// PublicKeyWire returns base64url(raw 32-byte public key) for HeartbeatResponse.
func (s *Signer) PublicKeyWire() string { return wire.EncodePublicKey(s.pub) }

// Mint signs claims into the wire token "signingInput.base64url(sig)".
func (s *Signer) Mint(c wire.Claims) (string, error) {
	in, err := wire.SigningInput(c)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(s.priv, []byte(in))
	return wire.AssembleToken(in, sig), nil
}

// MintReadGrant builds a donor-source / coordinator-dest read grant (D-M4.1-4).
// Direction is REVERSED vs the M4 repair token: SourceNodeID = donorID (the
// donor serves), DestNodeID = ReservedCoordinatorSourceID (the coordinator
// fetches). The not_before is now-5s, clamped to be no earlier than bootFloor
// so the donor's boot-floor check cannot reject a freshly minted grant.
func (s *Signer) MintReadGrant(donorID, cid, assignmentID string, generation, maxBytes int64, ttl time.Duration, now, bootFloor time.Time) (string, error) {
	jti, err := randomUUID()
	if err != nil {
		return "", fmt.Errorf("tokens: MintReadGrant random JTI: %w", err)
	}
	nb := now.Add(-5 * time.Second)
	if nb.Before(bootFloor) {
		nb = bootFloor
	}
	return s.Mint(wire.Claims{
		JTI:             jti,
		AssignmentID:    assignmentID,
		Generation:      generation,
		CID:             cid,
		SourceNodeID:    donorID,
		DestNodeID:      ReservedCoordinatorSourceID,
		NotBefore:       nb.Unix(),
		NotAfter:        now.Add(ttl).Unix(),
		MaxBytes:        maxBytes,
		ProtocolVersion: wire.ProtocolV1,
	})
}

// randomUUID returns a random RFC 4122 version-4 UUID string using crypto/rand.
func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
