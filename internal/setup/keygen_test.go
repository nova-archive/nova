// internal/setup/keygen_test.go
package setup

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestGenerateMasterKey(t *testing.T) {
	k1, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	raw, err := hex.DecodeString(k1)
	if err != nil || len(raw) != 32 {
		t.Fatalf("master key must be 64 hex chars / 32 bytes, got %d bytes (err=%v)", len(raw), err)
	}
	k2, _ := GenerateMasterKey()
	if k1 == k2 {
		t.Fatal("two generations must differ (CSPRNG)")
	}
}

func TestFingerprint(t *testing.T) {
	fp, err := Fingerprint("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp != "0011223344556677" {
		t.Fatalf("fingerprint = %q, want 0011223344556677", fp)
	}
	if _, err := Fingerprint("xyz"); err == nil {
		t.Fatal("Fingerprint must reject non-hex")
	}
}

func TestGenerateSwarmKey(t *testing.T) {
	sk, err := GenerateSwarmKey()
	if err != nil {
		t.Fatalf("GenerateSwarmKey: %v", err)
	}
	if !strings.HasPrefix(sk, "/key/swarm/psk/1.0.0/\n/base16/\n") {
		t.Fatalf("swarm key missing Kubo PSK header:\n%s", sk)
	}
	body := strings.TrimSpace(sk[strings.LastIndex(sk, "\n")+1:])
	if raw, err := hex.DecodeString(body); err != nil || len(raw) != 32 {
		t.Fatalf("swarm key body must be 32 bytes hex, got %d (err=%v)", len(raw), err)
	}
}

func TestGenerateSigningSeed(t *testing.T) {
	seed, err := GenerateSigningSeed()
	if err != nil {
		t.Fatalf("GenerateSigningSeed: %v", err)
	}
	if raw, err := hex.DecodeString(seed); err != nil || len(raw) != 32 {
		t.Fatalf("ed25519 seed must be 32 bytes hex, got %d (err=%v)", len(raw), err)
	}
}
