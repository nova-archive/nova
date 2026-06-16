package ca

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/transport"
)

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatal("no PEM")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGenerateCAAndIssue(t *testing.T) {
	caCertPEM, caKeyPEM, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	srvPEM, _, err := IssueServerCert(caCertPEM, caKeyPEM, ServerCertOptions{DNSNames: []string{"coordinator.local"}})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	cliPEM, _, err := IssueClientCert(caCertPEM, caKeyPEM, id, "donor-a")
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("ca pool")
	}
	for _, leafPEM := range [][]byte{srvPEM, cliPEM} {
		leaf := parseLeaf(t, leafPEM)
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}

	gotID, err := transport.IdentityFromCert(parseLeaf(t, cliPEM))
	if err != nil {
		t.Fatal(err)
	}
	if gotID.NodeID != id.String() {
		t.Fatalf("node id = %q want %q", gotID.NodeID, id.String())
	}
}

func TestIssueClientCertRejectsBadCA(t *testing.T) {
	if _, _, err := IssueClientCert([]byte("not pem"), []byte("not pem"), uuid.New(), "x"); err == nil {
		t.Fatal("expected error on bad CA material")
	}
}
