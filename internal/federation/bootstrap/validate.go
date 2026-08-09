package bootstrap

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io/fs"
	"strings"
)

// validateStaged checks the COMPLETE staged federation before anything becomes
// visible (D-M7.2-2a step 3). A failure here leaves the active federation
// exactly as it was — which is the whole reason validation precedes activation.
//
// It only validates what this run staged: artifacts preserved from an existing
// federation were already valid, and re-checking them would turn an unrelated
// pre-existing wart into a bootstrap failure.
func validateStaged(fsys fs.FS, p Params, stage *Stage) error {
	read := func(name string) ([]byte, bool) {
		if !stage.Has(name) {
			return nil, false
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, false
		}
		return b, true
	}

	// Every staged certificate must parse.
	for _, name := range []string{
		FileFederationCACert, FileCoordinatorCert, FileClientCert,
		FileNebulaCACert, FileLighthouseCert,
	} {
		b, ok := read(name)
		if !ok {
			continue
		}
		if _, err := parseCertPEM(b); err != nil {
			return fmt.Errorf("%s does not parse as a certificate: %w", name, err)
		}
	}

	// Every staged key must pair with its certificate. A mismatched pair is
	// silently useless at runtime — the TLS handshake simply never succeeds.
	for _, pair := range [][2]string{
		{FileFederationCACert, FileFederationCAKey},
		{FileCoordinatorCert, FileCoordinatorKey},
		{FileClientCert, FileClientKey},
		{FileNebulaCACert, FileNebulaCAKey},
		{FileLighthouseCert, FileLighthouseKey},
	} {
		certB, okC := read(pair[0])
		keyB, okK := read(pair[1])
		if !okC || !okK {
			continue
		}
		if err := certKeyPairs(certB, keyB); err != nil {
			return fmt.Errorf("%s and %s do not form a pair: %w", pair[0], pair[1], err)
		}
	}

	// The coordinator server certificate must actually name what was asked for,
	// or donors will fail the server-cert check with a confusing error.
	if b, ok := read(FileCoordinatorCert); ok {
		c, err := parseCertPEM(b)
		if err != nil {
			return err
		}
		if !hasDNSName(c, p.Hostname) {
			return fmt.Errorf("coordinator certificate does not name hostname %q (has %v)", p.Hostname, c.DNSNames)
		}
		if !hasIP(c, p.OperatorOverlayIP) {
			return fmt.Errorf("coordinator certificate does not name overlay IP %s (has %s)",
				p.OperatorOverlayIP, joinIPs(c.IPAddresses))
		}
	}

	// The repair seed must be a usable Ed25519 private key. Without it the
	// coordinator's source endpoint degrades to 503 and no grants are minted.
	if b, ok := read(FileRepairSigningKey); ok {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil {
			return fmt.Errorf("%s is not valid base64: %w", FileRepairSigningKey, err)
		}
		if len(raw) != ed25519.PrivateKeySize {
			return fmt.Errorf("%s is %d bytes, want an %d-byte Ed25519 private key",
				FileRepairSigningKey, len(raw), ed25519.PrivateKeySize)
		}
	}

	// The swarm key must be in Kubo's PSK format, or Kubo refuses to start and
	// the donor silently never joins the private swarm.
	if b, ok := read(FileSwarmKey); ok {
		if !strings.HasPrefix(string(b), "/key/swarm/psk/1.0.0/") {
			return fmt.Errorf("%s is not in Kubo PSK format", FileSwarmKey)
		}
	}

	return nil
}

// certKeyPairs verifies a PEM certificate and PEM private key belong together.
func certKeyPairs(certPEM, keyPEM []byte) error {
	c, err := parseCertPEM(certPEM)
	if err != nil {
		return err
	}
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return fmt.Errorf("key has no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return fmt.Errorf("key does not parse: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return fmt.Errorf("key is %T, want ed25519.PrivateKey", key)
	}
	pub, ok := c.PublicKey.(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("certificate public key is %T, want ed25519.PublicKey", c.PublicKey)
	}
	if !priv.Public().(ed25519.PublicKey).Equal(pub) {
		return fmt.Errorf("public keys differ")
	}
	return nil
}
