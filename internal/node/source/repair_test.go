package source

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
)

// destID is a SECOND donor — the repair destination — distinct from the source
// (donorID). A repair grant names donorID as source and destID as dest.
const destID = "22222222-2222-2222-2222-222222222222"

// makeCertFor builds a leaf with the given role and node-id URI SAN, so a repair
// destination donor can present an identity distinct from the source donor.
func makeCertFor(t *testing.T, role, nodeID string) *x509.Certificate {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: role},
		URIs:         []*url.URL{{Scheme: "nova", Host: role, Path: "/" + nodeID}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(nil, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// repairClaims is a donor↔donor grant: source = donorID (this server), dest =
// destID, with the additive Dest* binding fields populated.
func repairClaims(now time.Time) wire.Claims {
	c := validClaims(now)
	c.DestNodeID = destID
	c.DestAssignmentID = "dest-asg-1"
	c.DestGeneration = 2
	return c
}

func TestSourceServesRepairToMatchingDest(t *testing.T) {
	f := newFixture(t)
	tok := signToken(t, f.priv, repairClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCertFor(t, "node", destID)))
	if w.Code != http.StatusOK {
		t.Fatalf("repair serve code=%d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), f.pinner.body) {
		t.Fatalf("served %d bytes, want %d", w.Body.Len(), len(f.pinner.body))
	}
	if f.budget.took != 2048 {
		t.Fatalf("repair egress debited %d, want 2048", f.budget.took)
	}
}

func TestSourceRefusesMismatchedDest(t *testing.T) {
	f := newFixture(t)
	c := repairClaims(f.now)
	c.DestNodeID = "33333333-3333-3333-3333-333333333333" // grant is for a DIFFERENT donor
	tok := signToken(t, f.priv, c)
	w := f.do(reqWith(testCID, tok, makeCertFor(t, "node", destID)))
	if w.Code != http.StatusForbidden || decodeErr(t, w).Code != "source_unauthorized" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if f.pinner.getCalled {
		t.Fatal("must not open body for a mismatched dest")
	}
}

func TestSourceRefusesSourceAssignmentMismatch(t *testing.T) {
	f := newFixture(t)
	c := repairClaims(f.now)
	c.AssignmentID = "not-the-source-acked-assignment" // ≠ local progress record
	tok := signToken(t, f.priv, c)
	w := f.do(reqWith(testCID, tok, makeCertFor(t, "node", destID)))
	if w.Code != http.StatusNotFound || decodeErr(t, w).Code != "blob_unavailable" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRepairServeDebitsEgressRefusesOverBudget(t *testing.T) {
	f := newFixture(t)
	f.budget.allow = false
	tok := signToken(t, f.priv, repairClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCertFor(t, "node", destID)))
	if w.Code != http.StatusTooManyRequests || decodeErr(t, w).Code != "budget_exceeded" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if f.pinner.getCalled {
		t.Fatal("repair must not open body when egress budget is exhausted")
	}
}

func TestSourceStreamsExactlySizeNoOverwrite(t *testing.T) {
	f := newFixture(t)
	// Pinned object drifted LARGER than the recorded envelope size (2048).
	f.pinner.body = bytes.Repeat([]byte("Z"), 3000)
	tok := signToken(t, f.priv, validClaims(f.now)) // coordinator read path
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 2048 {
		t.Fatalf("served %d bytes, want exactly 2048 (never size+1)", w.Body.Len())
	}
	if f.budget.took != 2048 {
		t.Fatalf("debited %d, want 2048", f.budget.took)
	}
}
