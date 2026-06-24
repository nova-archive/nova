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

// Role is the federation peer role encoded in a leaf's URI SAN host.
type Role string

const (
	// RoleNode is the standard donor federation role (nova://node/<uuid>).
	RoleNode Role = "node"
	// RoleCoordinator is the coordinator acting as a federation client
	// (nova://coordinator/<uuid>), used when the coordinator pulls blobs from
	// donor read-source endpoints (P2-M4.1).
	RoleCoordinator Role = "coordinator"
)

// uriScheme + uriHost* define the URI SANs for federation peers:
//
//	nova://node/<uuid>        — donor client cert (M2)
//	nova://coordinator/<uuid> — coordinator client cert (P2-M4.1)
const (
	uriScheme          = "nova"
	uriHost            = "node" // kept for backward compat; mirrors uriHostNode
	uriHostNode        = "node"
	uriHostCoordinator = "coordinator"
)

// Identity is the verified federation identity of a peer.
type Identity struct {
	Role        Role   // RoleNode or RoleCoordinator
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

// IdentityFromCert extracts the role and node_id from a verified leaf's URI SAN
// and computes the DER fingerprint. It accepts:
//
//	nova://node/<uuid>        → {RoleNode, uuid, fp}
//	nova://coordinator/<uuid> → {RoleCoordinator, uuid, fp}
//
// Any other URI SAN host, or no matching SAN, returns an error.
// Minimal structural validation only — no UUID parsing (that is the coordinator's
// job, off the donor graph).
func IdentityFromCert(c *x509.Certificate) (Identity, error) {
	for _, u := range c.URIs {
		if u.Scheme != uriScheme {
			continue
		}
		var role Role
		switch u.Host {
		case uriHostNode:
			role = RoleNode
		case uriHostCoordinator:
			role = RoleCoordinator
		default:
			continue
		}
		id := strings.TrimPrefix(u.Path, "/")
		if id == "" {
			return Identity{}, fmt.Errorf("transport: empty node_id in %s://%s URI SAN", uriScheme, u.Host)
		}
		return Identity{Role: role, NodeID: id, Fingerprint: FingerprintDER(c)}, nil
	}
	return Identity{}, errors.New("transport: cert has no nova://node/<id> or nova://coordinator/<id> URI SAN")
}
