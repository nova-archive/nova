package setup

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionTLS_DevSelfSigned(t *testing.T) {
	dir := t.TempDir()
	a := validAnswers() // TLSMode = dev-self-signed
	res, err := ProvisionTLS(a, dir)
	if err != nil {
		t.Fatalf("ProvisionTLS: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(res.CertPath, res.KeyPath); err != nil {
		t.Fatalf("generated cert/key not loadable: %v", err)
	}
}

func TestProvisionTLS_Handoff(t *testing.T) {
	dir := t.TempDir()
	a := validAnswers()
	a.TLSMode = "dns-01"
	res, err := ProvisionTLS(a, dir)
	if err != nil {
		t.Fatalf("ProvisionTLS dns-01: %v", err)
	}
	if res.HandoffInstructions == "" {
		t.Fatal("dns-01 must return operator-handoff instructions")
	}
	if _, err := os.Stat(filepath.Join(dir, "fullchain.pem")); err == nil {
		t.Fatal("dns-01 must not generate a cert in M13")
	}
}
