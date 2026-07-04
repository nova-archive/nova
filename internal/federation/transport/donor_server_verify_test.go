package transport

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/ca"
)

// P2-M7 (cross-version drill finding): the coordinator's outbound donor
// connections verify the donor's SERVING cert by federation-CA chain + a
// nova:// URI SAN identity — never by hostname/ServerAuth EKU, which real
// donor federation certs (IssueClientCert) cannot satisfy. These tests prove
// the custom verifier is load-bearing in both directions: a real issued donor
// cert is ACCEPTED, and untrusted-CA / identity-less certs are REFUSED.

// startTLSServer serves one 200 OK over TLS with the given cert material.
func startTLSServer(t *testing.T, caPEM, certPEM, keyPEM []byte) (addr string, closeFn func()) {
	t.Helper()
	srvTLS, err := ServerTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := NewTLSListener(inner, srvTLS)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}
	go func() { _ = srv.Serve(ln) }()
	return inner.Addr().String(), func() { _ = srv.Close(); _ = ln.Close() }
}

func coordinatorClient(t *testing.T, caPEM, caKeyPEM []byte) *http.Client {
	t.Helper()
	cliPEM, cliKeyPEM, err := ca.IssueCoordinatorClientCert(caPEM, caKeyPEM, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	cliTLS, err := CoordinatorClientTLS(caPEM, cliPEM, cliKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cliTLS}}
}

func TestCoordinatorClientAcceptsRealDonorServingCert(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// The donor serves with its PRODUCTION federation cert: nova://node/<uuid>
	// URI SAN, ClientAuth EKU, no host SANs.
	donorPEM, donorKeyPEM, err := ca.IssueClientCert(caPEM, caKeyPEM, uuid.New(), "donor")
	if err != nil {
		t.Fatal(err)
	}
	addr, closeFn := startTLSServer(t, caPEM, donorPEM, donorKeyPEM)
	defer closeFn()

	resp, err := coordinatorClient(t, caPEM, caKeyPEM).Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("coordinator client must accept a federation-CA donor serving cert: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestCoordinatorClientRefusesUntrustedCA(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	rogueCA, rogueKey, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	roguePEM, rogueKeyPEM, err := ca.IssueClientCert(rogueCA, rogueKey, uuid.New(), "rogue")
	if err != nil {
		t.Fatal(err)
	}
	// Rogue server: presents a cert from a DIFFERENT CA; it must also accept
	// our client cert, so its ClientCAs pool is the real federation CA.
	srvTLS := &tls.Config{MinVersion: tls.VersionTLS12}
	cert, err := tls.X509KeyPair(roguePEM, rogueKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srvTLS.Certificates = []tls.Certificate{cert}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := NewTLSListener(inner, srvTLS)
	srv := &http.Server{Handler: http.NotFoundHandler()}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close(); _ = ln.Close() }()

	if resp, err := coordinatorClient(t, caPEM, caKeyPEM).Get("https://" + inner.Addr().String() + "/"); err == nil {
		resp.Body.Close()
		t.Fatal("coordinator client must refuse a serving cert from an untrusted CA")
	}
}

func TestCoordinatorClientRefusesIdentitylessCert(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// A federation-CA cert WITHOUT a nova:// URI SAN (a plain server cert):
	// chain-valid, but carries no federation identity — refused.
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{
		DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, closeFn := startTLSServer(t, caPEM, srvPEM, srvKeyPEM)
	defer closeFn()

	if resp, err := coordinatorClient(t, caPEM, caKeyPEM).Get("https://" + addr + "/"); err == nil {
		resp.Body.Close()
		t.Fatal("coordinator client must refuse a chain-valid cert with no nova:// identity")
	}
}
