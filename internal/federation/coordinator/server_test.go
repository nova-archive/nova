package coordinator

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func mustBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func TestServerListenServesOverMTLS(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}

	s := New(gen.New(pool), Config{
		ListenAddr: "127.0.0.1:0",
		Timers:     wire.ConfigUpdates{HeartbeatIntervalSeconds: 300},
		TLS:        TLSMaterial{CAPEM: caPEM, CertPEM: srvPEM, KeyPEM: srvKeyPEM},
	})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(runCtx)

	// Build a donor client.
	id := uuid.New()
	cliPEM, cliKeyPEM, err := ca.IssueClientCert(caPEM, caKeyPEM, id, "donor")
	if err != nil {
		t.Fatal(err)
	}
	cliTLS, err := transport.ClientTLSConfig(caPEM, cliPEM, cliKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cliTLS.ServerName = "localhost"
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cliTLS}, Timeout: 5 * time.Second}

	resp, err := hc.Post("https://"+s.Addr()+"/fed/v1/register", "application/json",
		mustBody(wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}}))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// A client with NO client cert cannot complete the mTLS handshake.
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{ServerName: "localhost", RootCAs: cliTLS.RootCAs}}, Timeout: 3 * time.Second}
	if _, err := noCert.Get("https://" + s.Addr() + "/fed/v1/register"); err == nil {
		t.Fatal("expected handshake failure without client cert")
	}
}
