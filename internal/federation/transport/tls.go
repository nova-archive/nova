package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
)

// caPool parses a PEM CA bundle into a CertPool.
func caPool(caPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("transport: no certificates parsed from CA PEM")
	}
	return pool, nil
}

// ServerTLSConfig builds the coordinator federation listener's TLS config:
// present the server cert, and REQUIRE + VERIFY client certs against the
// federation CA. TLS 1.2 floor (Ed25519 leaves require 1.2+; 1.3 preferred).
func ServerTLSConfig(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("transport: server keypair: %w", err)
	}
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig builds the donor's mTLS client config: present the client
// cert, verify the coordinator's server cert against the federation CA.
func ClientTLSConfig(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("transport: client keypair: %w", err)
	}
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// CoordinatorClientTLS builds the coordinator's mTLS client config for outbound
// connections to donor read-source/audit endpoints (P2-M4.1/M6), presenting the
// nova://coordinator/<uuid> identity.
//
// Donor endpoints serve TLS with the donor's FEDERATION cert — issued by
// `novactl node issue` with a nova://node/<uuid> URI SAN, ClientAuth EKU, and
// NO host SANs — so Go's standard hostname+ServerAuth verification can never
// pass against a real donor (P2-M7 cross-version drill finding: every
// donor-backed read failed `tls: bad certificate`). Verification is therefore
// REPLACED (not removed): the chain must verify against the federation CA and
// the leaf must carry a nova:// federation URI SAN — the same identity model
// the federation listener enforces for clients. Byte integrity never rests on
// TLS here: reads re-import and require CID equality; audits reconstruct the
// block CID from the returned bytes.
func CoordinatorClientTLS(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("transport: coordinator client keypair: %w", err)
	}
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		RootCAs:               pool,
		MinVersion:            tls.VersionTLS12,
		InsecureSkipVerify:    true, // custom verification below — never a verification bypass
		VerifyPeerCertificate: verifyFederationServerChain(pool),
	}, nil
}

// verifyFederationServerChain verifies a donor server chain against the
// federation CA and requires a nova:// federation URI SAN identity on the
// leaf. EKU is deliberately unconstrained: donor federation certs carry
// ClientAuth only, and this plane's authorization derives from the URI SAN
// identity plus per-request signed grants, not from certificate EKU bits.
func verifyFederationServerChain(pool *x509.CertPool) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("transport: donor presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("transport: donor leaf: %w", err)
		}
		inters := x509.NewCertPool()
		for _, raw := range rawCerts[1:] {
			if c, cerr := x509.ParseCertificate(raw); cerr == nil {
				inters.AddCert(c)
			}
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:         pool,
			Intermediates: inters,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		}); err != nil {
			return fmt.Errorf("transport: donor server cert not signed by the federation CA: %w", err)
		}
		if _, err := IdentityFromCert(leaf); err != nil {
			return fmt.Errorf("transport: donor server cert has no federation identity: %w", err)
		}
		return nil
	}
}

// NewTLSListener wraps a net.Listener in a TLS listener using cfg. Used by both
// the coordinator federation server and tests.
func NewTLSListener(inner net.Listener, cfg *tls.Config) net.Listener {
	return tls.NewListener(inner, cfg)
}
