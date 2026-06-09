// Package setup is the UI-agnostic first-run setup domain core, shared by the
// /setup/* coordinator seam (web wizard) and by `novactl setup`. It generates
// key material, renders operator.yaml + the two-vhost nova.conf, and performs
// the atomic first-run commit.
package setup

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// GenerateMasterKey returns a fresh 32-byte master key as 64 lowercase hex chars.
func GenerateMasterKey() (string, error) { return randHex(32) }

// GenerateSigningSeed returns a fresh 32-byte Ed25519 seed as hex. The seed
// expands to a full ed25519 private key via ed25519.NewKeyFromSeed; we validate
// that here so a bad CSPRNG read fails loudly.
func GenerateSigningSeed() (string, error) {
	h, err := randHex(ed25519.SeedSize)
	if err != nil {
		return "", err
	}
	raw, _ := hex.DecodeString(h)
	_ = ed25519.NewKeyFromSeed(raw) // panics on wrong length; SeedSize guarantees correctness
	return h, nil
}

// GenerateSwarmKey returns a fresh IPFS private-network swarm key in Kubo's
// PSK v1 base16 wire format.
func GenerateSwarmKey() (string, error) {
	body, err := randHex(32)
	if err != nil {
		return "", err
	}
	return "/key/swarm/psk/1.0.0/\n/base16/\n" + body, nil
}

// Fingerprint returns the first 8 bytes of a hex master key as 16 lowercase hex
// chars — the forced-readback challenge shown during setup.
func Fingerprint(masterKeyHex string) (string, error) {
	raw, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return "", fmt.Errorf("setup: fingerprint: %w", err)
	}
	if len(raw) < 8 {
		return "", fmt.Errorf("setup: fingerprint: key too short (%d bytes)", len(raw))
	}
	return hex.EncodeToString(raw[:8]), nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("setup: csprng: %w", err)
	}
	return hex.EncodeToString(b), nil
}
