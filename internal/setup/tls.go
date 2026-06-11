package setup

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// TLSResult reports the cert/key paths the rendered nova.conf should point at,
// plus any operator-handoff instructions for modes M13 does not fully automate.
type TLSResult struct {
	CertPath            string
	KeyPath             string
	HandoffInstructions string
}

// ProvisionTLS prepares TLS material for the chosen mode, writing into tlsDir
// where applicable.
func ProvisionTLS(a Answers, tlsDir string) (TLSResult, error) {
	switch a.TLSMode {
	case "dev-self-signed":
		return provisionSelfSigned(a, tlsDir)
	case "static":
		return provisionStatic(a)
	case "http-01":
		return TLSResult{
			CertPath: filepath.Join(tlsDir, "fullchain.pem"),
			KeyPath:  filepath.Join(tlsDir, "privkey.pem"),
			HandoffInstructions: "http-01: the certbot prod profile obtains a certificate " +
				"via the ACME HTTP-01 webroot challenge. Certificate Transparency (CT) log " +
				"disclosure of the hostname (" + a.Hostname + ") is expected. " +
				"Issuance and renewal-reload are fully automated in the prod profile " +
				"(the certbot sidecar issues on first boot; nginx reloads on renewal) — " +
				"no manual certbot step is required.",
		}, nil
	case "dns-01":
		return TLSResult{
			HandoffInstructions: "dns-01: provide your DNS provider API credentials and run " +
				"the appropriate certbot DNS plugin out-of-band " +
				"(e.g. `certbot certonly --dns-<provider>`). " +
				"Once the certificate is obtained, either point tls.cert_path/key_path at the " +
				"certbot live directory, or copy fullchain.pem and privkey.pem into the tls dir " +
				"(" + tlsDir + ") and restart Nova.",
		}, nil
	case "onion":
		return TLSResult{
			HandoffInstructions: "onion: ensure Tor is installed and running with a HiddenService " +
				"directive pointing at Nova's listener. Generate (or supply) a self-signed " +
				"certificate for the .onion vhost, then set tls.cert_path/key_path to its paths " +
				"and restart Nova.",
		}, nil
	default:
		return TLSResult{}, fmt.Errorf("setup: tls: unknown tls_mode %q (should be unreachable — Answers.Validate gates this)", a.TLSMode)
	}
}

// provisionSelfSigned generates a self-signed CA and a leaf certificate signed
// by that CA, writing fullchain.pem and privkey.pem into tlsDir.
func provisionSelfSigned(a Answers, tlsDir string) (TLSResult, error) {
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: mkdir %s: %w", tlsDir, err)
	}

	// --- CA key + cert ---
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: generate CA key: %w", err)
	}

	caSerial, err := randSerial()
	if err != nil {
		return TLSResult{}, err
	}

	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: caSerial,
		Subject: pkix.Name{
			CommonName:   "Nova Dev CA",
			Organization: []string{"Nova Self-Signed"},
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: create CA cert: %w", err)
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: parse CA cert: %w", err)
	}

	// --- leaf key + cert ---
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: generate leaf key: %w", err)
	}

	leafSerial, err := randSerial()
	if err != nil {
		return TLSResult{}, err
	}

	leafTemplate := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject: pkix.Name{
			CommonName: a.Hostname,
		},
		NotBefore:   now.Add(-time.Minute),
		NotAfter:    now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// If hostname is an IP address, add it to IPAddresses; otherwise DNSNames.
	if ip := net.ParseIP(a.Hostname); ip != nil {
		leafTemplate.IPAddresses = []net.IP{ip}
	} else {
		leafTemplate.DNSNames = []string{a.Hostname}
	}

	leafCertDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: create leaf cert: %w", err)
	}

	// --- write fullchain.pem = leaf PEM then CA PEM ---
	certPath := filepath.Join(tlsDir, "fullchain.pem")
	cf, err := os.OpenFile(certPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: create fullchain.pem: %w", err)
	}
	defer cf.Close()

	if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: leafCertDER}); err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: write leaf cert PEM: %w", err)
	}
	if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: caCertDER}); err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: write CA cert PEM: %w", err)
	}

	// --- write privkey.pem = leaf ECDSA private key ---
	keyPath := filepath.Join(tlsDir, "privkey.pem")
	kf, err := os.OpenFile(keyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: create privkey.pem: %w", err)
	}
	defer kf.Close()

	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: marshal leaf key: %w", err)
	}
	if err := pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}); err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: write key PEM: %w", err)
	}

	// Verify the pair loads correctly before returning.
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: self-check LoadX509KeyPair failed: %w", err)
	}

	return TLSResult{CertPath: certPath, KeyPath: keyPath}, nil
}

// provisionStatic validates that the user-supplied cert/key are readable and
// parse correctly; it does not copy or generate any files.
func provisionStatic(a Answers) (TLSResult, error) {
	if _, err := os.Stat(a.CertPath); err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: static cert_path %q: %w", a.CertPath, err)
	}
	if _, err := os.Stat(a.KeyPath); err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: static key_path %q: %w", a.KeyPath, err)
	}
	if _, err := tls.LoadX509KeyPair(a.CertPath, a.KeyPath); err != nil {
		return TLSResult{}, fmt.Errorf("setup: tls: static cert/key pair invalid: %w", err)
	}
	return TLSResult{CertPath: a.CertPath, KeyPath: a.KeyPath}, nil
}

// randSerial returns a random 128-bit certificate serial number.
func randSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("setup: tls: generate serial: %w", err)
	}
	return serial, nil
}
