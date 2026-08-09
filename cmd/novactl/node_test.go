package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeCAInitAndIssue(t *testing.T) {
	dir := t.TempDir()
	if err := cmdNode([]string{"ca-init", "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"federation-ca.crt", "federation-ca.key", "coordinator-federation.crt", "coordinator-federation.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	out := filepath.Join(dir, "donor-a")
	if err := cmdNode([]string{"issue", "--dir", dir, "--name", "donor-a", "--out", out}); err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(filepath.Join(out, "federation.crt"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	c, _ := x509.ParseCertificate(blk.Bytes)
	if len(c.URIs) == 0 || c.URIs[0].Scheme != "nova" {
		t.Fatalf("client cert missing nova URI SAN: %+v", c.URIs)
	}
}

func TestNovactlIssuesCoordinatorClientCert(t *testing.T) {
	dir := t.TempDir()
	// First initialise a CA.
	if err := cmdNode([]string{"ca-init", "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "coordinator-client")
	if err := cmdNode([]string{"issue-coordinator-client", "--dir", dir, "--out", out}); err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(filepath.Join(out, "federation-client.crt"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatal("no PEM block in coordinator client cert")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	if len(c.URIs) == 0 {
		t.Fatal("coordinator client cert has no URI SANs")
	}
	u := c.URIs[0]
	if u.Scheme != "nova" || u.Host != "coordinator" {
		t.Fatalf("expected nova://coordinator/<uuid> SAN, got %s", u.String())
	}
	id := u.Path
	if len(id) < 2 { // at minimum "/x"
		t.Fatalf("coordinator id too short: %q", id)
	}
}

const testPinnedNodeImage = "ghcr.io/nova-archive/nova-node@sha256:" +
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestNodeNebulaTemplate(t *testing.T) {
	dir := t.TempDir()
	if err := cmdNode([]string{
		"nebula-template", "--name", "donor-a", "--nebula-ip", "10.42.0.23/24",
		"--image", testPinnedNodeImage, "--out", dir,
	}); err != nil {
		t.Fatal(err)
	}
	// P2-M7.2: the canonical topology is nebula + hardened kubo + nova-node, so
	// the bundle now includes the Kubo hardening script, and the operator notes
	// are README.md.
	for _, f := range []string{"nebula-config.yml", "node.yaml", "compose.yaml", "kubo-init.sh", "README.md"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
		if len(b) == 0 {
			t.Fatalf("%s empty", f)
		}
	}
}

// TestNodeNebulaTemplateRefusesMutableTag pins the D-M7.2-5 invariant at the
// CLI boundary: a generated operational artifact must never carry a mutable
// tag, because the operator and the volunteer have to run the same bytes.
func TestNodeNebulaTemplateRefusesMutableTag(t *testing.T) {
	dir := t.TempDir()
	err := cmdNode([]string{
		"nebula-template", "--name", "donor-a", "--nebula-ip", "10.42.0.23/24",
		"--image", "ghcr.io/nova-archive/nova-node:latest", "--out", dir,
	})
	if err == nil {
		t.Fatal("a mutable tag must be refused")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error should name the digest requirement, got %v", err)
	}
}
