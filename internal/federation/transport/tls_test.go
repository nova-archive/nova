package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// pemPair returns (certPEM, keyPEM) for a cert signed by (parent, parentKey), or
// a self-signed CA when parent is nil. uri is an optional URI SAN.
func pemPair(t *testing.T, parent *x509.Certificate, parentKey ed25519.PrivateKey, isCA bool, uri string, dns []string) ([]byte, []byte, *x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "t"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	}
	tmpl.DNSNames = dns
	if uri != "" {
		u, _ := url.Parse(uri)
		tmpl.URIs = []*url.URL{u}
	}
	signer, signerKey := tmpl, priv
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, pub, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, cert, priv
}

func TestMutualTLSHandshake(t *testing.T) {
	caPEM, _, caCert, caKey := pemPair(t, nil, nil, true, "", nil)
	srvPEM, srvKeyPEM, _, _ := pemPair(t, caCert, caKey, false, "", []string{"localhost"})
	cliPEM, cliKeyPEM, _, _ := pemPair(t, caCert, caKey, false, "nova://node/abc", nil)

	srvTLS, err := ServerTLSConfig(caPEM, srvPEM, srvKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cliTLS, err := ClientTLSConfig(caPEM, cliPEM, cliKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cliTLS.ServerName = "localhost"

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.TLS.PeerCertificates) == 0 {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, "ok")
		}),
		TLSConfig: srvTLS,
	}
	tlsLn := NewTLSListener(ln, srvTLS)
	go srv.Serve(tlsLn)
	defer srv.Close()

	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cliTLS}, Timeout: 3 * time.Second}
	resp, err := c.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("handshake/get failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = context.Background()
}
