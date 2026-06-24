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
// connections to donor read-source endpoints (P2-M4.1). It is structurally
// identical to ClientTLSConfig but is named distinctly so call sites are clear
// about which identity is being presented (nova://coordinator/<uuid> cert).
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
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// NewTLSListener wraps a net.Listener in a TLS listener using cfg. Used by both
// the coordinator federation server and tests.
func NewTLSListener(inner net.Listener, cfg *tls.Config) net.Listener {
	return tls.NewListener(inner, cfg)
}
