package transport

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/ca"
)

// P2-M7.1 (D-M7.1-4): the donor's outbound donor↔donor repair connections
// verify the SOURCE donor's serving cert by federation-CA chain + the EXACT
// nova://node/<id> URI SAN named by the repair instruction — never by
// hostname/ServerAuth, and never "any valid federation cert". These tests
// prove the binding is load-bearing in both directions: the expected source
// identity is ACCEPTED, and wrong-identity / identity-less / untrusted-CA /
// wrong-role certs are all REFUSED.

// repairClient builds the destination donor's repair HTTP client: base
// federation client config (ClientTLSConfig) derived into an
// identity-pinned repair config expecting expectedNodeID.
func repairClient(t *testing.T, caPEM, caKeyPEM []byte, expectedNodeID string) *http.Client {
	t.Helper()
	destPEM, destKeyPEM, err := ca.IssueClientCert(caPEM, caKeyPEM, uuid.New(), "dest-donor")
	if err != nil {
		t.Fatal(err)
	}
	base, err := ClientTLSConfig(caPEM, destPEM, destKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := DonorRepairClientTLS(base, expectedNodeID)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
}

func TestDonorRepairClientAcceptsExpectedSourceIdentity(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// The source donor serves with its PRODUCTION federation cert:
	// nova://node/<uuid> URI SAN, ClientAuth EKU, no host SANs.
	srcID := uuid.New()
	srcPEM, srcKeyPEM, err := ca.IssueClientCert(caPEM, caKeyPEM, srcID, "src-donor")
	if err != nil {
		t.Fatal(err)
	}
	addr, closeFn := startTLSServer(t, caPEM, srcPEM, srcKeyPEM)
	defer closeFn()

	resp, err := repairClient(t, caPEM, caKeyPEM, srcID.String()).Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("repair client must accept the instruction-named source's federation cert: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestDonorRepairClientRefusesWrongSourceIdentity(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// A DIFFERENT enrolled donor: chain-valid federation cert, valid
	// nova://node/<uuid> identity — but not the source the repair
	// instruction named. Must be refused (no "any federation cert" pass).
	otherPEM, otherKeyPEM, err := ca.IssueClientCert(caPEM, caKeyPEM, uuid.New(), "other-donor")
	if err != nil {
		t.Fatal(err)
	}
	addr, closeFn := startTLSServer(t, caPEM, otherPEM, otherKeyPEM)
	defer closeFn()

	expected := uuid.New().String()
	if resp, err := repairClient(t, caPEM, caKeyPEM, expected).Get("https://" + addr + "/"); err == nil {
		resp.Body.Close()
		t.Fatal("repair client must refuse a federation cert whose node identity is not the instruction-named source")
	}
}

func TestDonorRepairClientRefusesHostnameOnlyCert(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// A federation-CA SERVER cert with host SANs and ServerAuth EKU but no
	// nova:// identity: standard hostname verification would accept it —
	// the repair verifier must not (no hostname/ServerAuth fallback).
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{
		DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, closeFn := startTLSServer(t, caPEM, srvPEM, srvKeyPEM)
	defer closeFn()

	if resp, err := repairClient(t, caPEM, caKeyPEM, uuid.New().String()).Get("https://" + addr + "/"); err == nil {
		resp.Body.Close()
		t.Fatal("repair client must refuse a chain-valid cert with no nova:// identity")
	}
}

func TestDonorRepairClientRefusesUntrustedCA(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	rogueCA, rogueKey, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// The rogue cert claims the EXPECTED node identity — but chains to a
	// different CA. Identity claims mean nothing off the federation chain.
	srcID := uuid.New()
	roguePEM, rogueKeyPEM, err := ca.IssueClientCert(rogueCA, rogueKey, srcID, "impostor")
	if err != nil {
		t.Fatal(err)
	}
	// Serve with the ROGUE chain but accept clients from the real CA so the
	// handshake reaches the client's verification step.
	addr, closeFn := startTLSServer(t, caPEM, roguePEM, rogueKeyPEM)
	defer closeFn()

	if resp, err := repairClient(t, caPEM, caKeyPEM, srcID.String()).Get("https://" + addr + "/"); err == nil {
		resp.Body.Close()
		t.Fatal("repair client must refuse a correct-identity cert from an untrusted CA")
	}
}

func TestDonorRepairClientRefusesCoordinatorRoleCert(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// Same UUID, wrong ROLE: nova://coordinator/<id> is not nova://node/<id>.
	srcID := uuid.New()
	coordPEM, coordKeyPEM, err := ca.IssueCoordinatorClientCert(caPEM, caKeyPEM, srcID)
	if err != nil {
		t.Fatal(err)
	}
	addr, closeFn := startTLSServer(t, caPEM, coordPEM, coordKeyPEM)
	defer closeFn()

	if resp, err := repairClient(t, caPEM, caKeyPEM, srcID.String()).Get("https://" + addr + "/"); err == nil {
		resp.Body.Close()
		t.Fatal("repair client must refuse a coordinator-role cert even with a matching id")
	}
}

func TestDonorRepairClientTLSFailsClosed(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	cliPEM, cliKeyPEM, err := ca.IssueClientCert(caPEM, caKeyPEM, uuid.New(), "donor")
	if err != nil {
		t.Fatal(err)
	}
	base, err := ClientTLSConfig(caPEM, cliPEM, cliKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DonorRepairClientTLS(nil, uuid.New().String()); err == nil {
		t.Fatal("nil base config must be refused")
	}
	if _, err := DonorRepairClientTLS(base, ""); err == nil {
		t.Fatal("empty expected node_id must be refused")
	}
}
