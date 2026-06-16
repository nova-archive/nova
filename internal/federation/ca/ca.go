// Package ca is the Nova federation X.509 certificate authority used by
// `novactl node` to issue the coordinator's federation server cert and donor
// federation client certs. Ed25519 keys, pure crypto/x509 — no external binary
// and no Nebula involvement (Nebula has its own separate cert system). This
// package is OPERATOR-SIDE and must never enter the cmd/node build graph.
package ca

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"

	"github.com/google/uuid"
)

const (
	caValidity     = 10 * 365 * 24 * time.Hour
	leafValidity   = 2 * 365 * 24 * time.Hour
	uriSANTemplate = "nova://node/%s"
)

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func encodePair(certDER []byte, key ed25519.PrivateKey) (certPEM, keyPEM []byte, err error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// GenerateCA creates a self-signed Ed25519 federation CA.
func GenerateCA() (certPEM, keyPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "Nova Federation CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, err
	}
	return encodePair(der, priv)
}

// parseCA decodes a CA cert + key PEM pair.
func parseCA(caCertPEM, caKeyPEM []byte) (*x509.Certificate, ed25519.PrivateKey, error) {
	cblk, _ := pem.Decode(caCertPEM)
	kblk, _ := pem.Decode(caKeyPEM)
	if cblk == nil || kblk == nil {
		return nil, nil, fmt.Errorf("ca: malformed CA PEM")
	}
	caCert, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: parse CA cert: %w", err)
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(kblk.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: parse CA key: %w", err)
	}
	key, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("ca: CA key is not Ed25519")
	}
	return caCert, key, nil
}

// ServerCertOptions configures the coordinator's federation server cert.
type ServerCertOptions struct {
	DNSNames    []string
	IPAddresses []string // may include the coordinator's Nebula overlay IP, as strings
}

func issueLeaf(caCertPEM, caKeyPEM []byte, tmpl *x509.Certificate) (certPEM, keyPEM []byte, err error) {
	caCert, caKey, err := parseCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl.SerialNumber = sn
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(leafValidity)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.BasicConstraintsValid = true
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, pub, caKey)
	if err != nil {
		return nil, nil, err
	}
	return encodePair(der, priv)
}

// IssueServerCert issues the coordinator's federation server cert.
func IssueServerCert(caCertPEM, caKeyPEM []byte, opts ServerCertOptions) (certPEM, keyPEM []byte, err error) {
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "nova-coordinator-federation"},
		DNSNames:    opts.DNSNames,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, ip := range opts.IPAddresses {
		if parsed := net.ParseIP(ip); parsed != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, parsed)
		}
	}
	return issueLeaf(caCertPEM, caKeyPEM, tmpl)
}

// IssueClientCert issues a donor federation client cert binding node_id in the
// URI SAN (nova://node/<uuid>).
func IssueClientCert(caCertPEM, caKeyPEM []byte, nodeID uuid.UUID, displayName string) (certPEM, keyPEM []byte, err error) {
	u, err := url.Parse(fmt.Sprintf(uriSANTemplate, nodeID.String()))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: displayName},
		URIs:        []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return issueLeaf(caCertPEM, caKeyPEM, tmpl)
}
