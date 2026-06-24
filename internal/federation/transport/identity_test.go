package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// testLeaf builds a self-signed leaf carrying the given URI SAN (or none).
func testLeaf(t *testing.T, uri string) *x509.Certificate {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	if uri != "" {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func TestIdentityFromCert(t *testing.T) {
	c := testLeaf(t, "nova://node/550e8400-e29b-41d4-a716-446655440000")
	id, err := IdentityFromCert(c)
	if err != nil {
		t.Fatal(err)
	}
	if id.NodeID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("node id = %q", id.NodeID)
	}
	if len(id.Fingerprint) != len("sha256:")+64 || id.Fingerprint[:7] != "sha256:" {
		t.Fatalf("fingerprint = %q", id.Fingerprint)
	}
	if FingerprintDER(c) != id.Fingerprint {
		t.Fatal("fingerprint mismatch")
	}
}

func TestIdentityFromCertNoSAN(t *testing.T) {
	if _, err := IdentityFromCert(testLeaf(t, "")); err == nil {
		t.Fatal("expected error for cert without nova://node SAN")
	}
}

func TestIdentityFromCertWrongScheme(t *testing.T) {
	if _, err := IdentityFromCert(testLeaf(t, "https://example.com/x")); err == nil {
		t.Fatal("expected error for non-nova URI SAN")
	}
}

// TestIdentityRole covers the three new role-parsing cases (P2-M4.1):
//   - nova://coordinator/<uuid> → RoleCoordinator
//   - nova://node/<uuid>        → RoleNode (existing behavior preserved)
//   - missing/unknown SAN       → error
func TestIdentityRole(t *testing.T) {
	t.Run("coordinator SAN yields RoleCoordinator", func(t *testing.T) {
		c := testLeaf(t, "nova://coordinator/aaaabbbb-cccc-dddd-eeee-ffffffffffff")
		id, err := IdentityFromCert(c)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id.Role != RoleCoordinator {
			t.Fatalf("role = %q, want %q", id.Role, RoleCoordinator)
		}
		if id.NodeID != "aaaabbbb-cccc-dddd-eeee-ffffffffffff" {
			t.Fatalf("node id = %q", id.NodeID)
		}
	})

	t.Run("node SAN yields RoleNode", func(t *testing.T) {
		c := testLeaf(t, "nova://node/550e8400-e29b-41d4-a716-446655440000")
		id, err := IdentityFromCert(c)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id.Role != RoleNode {
			t.Fatalf("role = %q, want %q", id.Role, RoleNode)
		}
	})

	t.Run("no SAN yields error", func(t *testing.T) {
		if _, err := IdentityFromCert(testLeaf(t, "")); err == nil {
			t.Fatal("expected error for cert without nova URI SAN")
		}
	})

	t.Run("unknown nova host yields error", func(t *testing.T) {
		if _, err := IdentityFromCert(testLeaf(t, "nova://unknown/abc")); err == nil {
			t.Fatal("expected error for unknown nova URI host")
		}
	})
}
