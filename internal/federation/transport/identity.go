// Package transport holds the shared mTLS-over-Nebula helpers: building the
// client/server *tls.Config against the federation CA, and extracting a donor's
// stable identity from a verified federation certificate. It is pure stdlib
// (crypto/tls, crypto/x509) so it stays inside the donor dependency boundary;
// it imports NO uuid library — node_id is carried as an opaque validated string.
package transport

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// uriScheme + uriHost define the URI SAN that carries a donor's node_id:
// nova://node/<uuid>. The UUID is minted by `novactl node issue` at cert-issue
// time and is the stable identity; the coordinator parses it as a real UUID.
const (
	uriScheme = "nova"
	uriHost   = "node"
)

// Identity is the verified federation identity of a peer.
type Identity struct {
	NodeID      string // the URI-SAN UUID string (validated structurally, not parsed)
	Fingerprint string // "sha256:" + hex(sha256(leaf DER))
}

// FingerprintDER returns the canonical fingerprint of a certificate: the SHA-256
// of its DER encoding, hex-encoded, prefixed "sha256:". DER (not SPKI) so the
// fingerprint tracks the certificate — correct for rotation/revocation
// bookkeeping.
func FingerprintDER(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// IdentityFromCert extracts the node_id (from the nova://node/<uuid> URI SAN)
// and the DER fingerprint from a verified leaf. It does minimal structural
// validation only — no uuid parsing (that is the coordinator's job, off the
// donor graph).
func IdentityFromCert(c *x509.Certificate) (Identity, error) {
	for _, u := range c.URIs {
		if u.Scheme != uriScheme || u.Host != uriHost {
			continue
		}
		id := strings.TrimPrefix(u.Path, "/")
		if id == "" {
			return Identity{}, errors.New("transport: empty node_id in URI SAN")
		}
		return Identity{NodeID: id, Fingerprint: FingerprintDER(c)}, nil
	}
	return Identity{}, fmt.Errorf("transport: cert has no %s://%s/<id> URI SAN", uriScheme, uriHost)
}
