package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// P2-M7.1 (D-M7.1-4): HTTPClient.Fetch on the donor↔donor path must verify the
// SOURCE donor's serving cert by federation-CA chain + the exact
// nova://node/<src.NodeID> URI SAN from the repair instruction. A source donor
// serves TLS with its federation cert (URI SAN only, no host SANs), so any
// hostname-verified client would fail against a real source — and any
// "any-federation-cert" client would accept an impostor donor.

// startSourceDonorTLS serves GET /fed/v1/blob/{cid} over mTLS with certPEM as
// its serving cert, returning the payload and echoing back the repair token it
// saw via gotToken.
func startSourceDonorTLS(t *testing.T, caPEM, certPEM, keyPEM []byte, payload string, gotToken *string) (addr string, closeFn func()) {
	t.Helper()
	srvTLS, err := transport.ServerTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := transport.NewTLSListener(inner, srvTLS)
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*gotToken = r.Header.Get("X-Nova-Repair-Token")
			_, _ = io.WriteString(w, payload)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	return inner.Addr().String(), func() { _ = srv.Close(); _ = ln.Close() }
}

// donorFetchClient builds the destination donor's HTTPClient exactly as
// cmd/node does: base federation client config via transport.ClientTLSConfig.
func donorFetchClient(t *testing.T, caPEM, caKeyPEM []byte) *HTTPClient {
	t.Helper()
	destPEM, destKeyPEM, err := ca.IssueClientCert(caPEM, caKeyPEM, uuid.New(), "dest-donor")
	if err != nil {
		t.Fatal(err)
	}
	base, err := transport.ClientTLSConfig(caPEM, destPEM, destKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return NewHTTPClient("https://coordinator.invalid", base)
}

func TestFetchDonorSourceVerifiesFederationIdentity(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// The source donor serves with its PRODUCTION federation cert:
	// nova://node/<srcID>, ClientAuth EKU, no host SANs.
	srcID := uuid.New()
	srcPEM, srcKeyPEM, err := ca.IssueClientCert(caPEM, caKeyPEM, srcID, "src-donor")
	if err != nil {
		t.Fatal(err)
	}
	var gotToken string
	addr, closeFn := startSourceDonorTLS(t, caPEM, srcPEM, srcKeyPEM, "envelope-bytes", &gotToken)
	defer closeFn()

	c := donorFetchClient(t, caPEM, caKeyPEM)

	// Correct instruction-named identity: must succeed.
	rc, err := c.Fetch(context.Background(), wire.ChangeSource{
		NodeID: srcID.String(), NebulaAddr: addr, Token: "tok-1",
	}, "bafyX", 1<<20)
	if err != nil {
		t.Fatalf("fetch from the instruction-named source must succeed: %v", err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(body) != "envelope-bytes" {
		t.Fatalf("body = %q, err = %v", body, err)
	}
	if gotToken != "tok-1" {
		t.Fatalf("repair token = %q, want tok-1", gotToken)
	}

	// Same server, but the instruction names a DIFFERENT source node: the
	// handshake must refuse — a valid federation cert is not enough.
	if rc, err := c.Fetch(context.Background(), wire.ChangeSource{
		NodeID: uuid.New().String(), NebulaAddr: addr, Token: "tok-2",
	}, "bafyX", 1<<20); err == nil {
		rc.Close()
		t.Fatal("fetch must refuse a source whose identity is not the instruction-named node")
	}
}

func TestFetchDonorSourceRefusesHostnameOnlyCert(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// Chain-valid server cert with host SANs but NO nova:// identity —
	// hostname verification would accept it; the repair client must not.
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{
		DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotToken string
	addr, closeFn := startSourceDonorTLS(t, caPEM, srvPEM, srvKeyPEM, "x", &gotToken)
	defer closeFn()

	c := donorFetchClient(t, caPEM, caKeyPEM)
	if rc, err := c.Fetch(context.Background(), wire.ChangeSource{
		NodeID: uuid.New().String(), NebulaAddr: addr, Token: "tok",
	}, "bafyX", 1<<20); err == nil {
		rc.Close()
		t.Fatal("fetch must refuse an identity-less cert even if hostname verification would pass")
	}
}
