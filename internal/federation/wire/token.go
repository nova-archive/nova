package wire

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Repair-token verification errors (D1). M1 ships Verify; the coordinator-side
// MINT FLOW and the source-side single-use jti replay cache land in M4 — Verify
// itself does NOT enforce replay.
var (
	ErrMalformedToken  = errors.New("wire: malformed token")
	ErrMalformedClaims = errors.New("wire: malformed claims")
	ErrBadSignature    = errors.New("wire: bad signature")
	ErrNotYetValid     = errors.New("wire: token not yet valid")
	ErrExpired         = errors.New("wire: token expired")
)

// Claims is the Ed25519 repair-token payload (D1). In M1 the id/cid fields are
// opaque non-empty strings; deeper CID/UUID parsing lands when transfer/register
// code needs it (no go-cid/UUID dependency enters the donor graph yet).
type Claims struct {
	JTI             string `json:"jti"`
	AssignmentID    string `json:"assignment_id"`
	Generation      int64  `json:"generation"`
	CID             string `json:"cid"`
	SourceNodeID    string `json:"source_node_id"`
	DestNodeID      string `json:"dest_node_id"`
	NotBefore       int64  `json:"not_before"`
	NotAfter        int64  `json:"not_after"`
	MaxBytes        int64  `json:"max_bytes"`
	ProtocolVersion string `json:"protocol_version"`
}

var b64 = base64.RawURLEncoding

// SigningInput returns the canonical signing input for a token:
// base64url(claims_json). claims_json is deterministic because Claims marshals
// in fixed struct-field order. The coordinator signs []byte(SigningInput) with
// its Ed25519 PRIVATE key in the coordinator-only internal/federation/tokens
// package (M4). This shared package holds NO private-key / minting API — donors
// only ever Verify.
func SigningInput(c Claims) (string, error) {
	cj, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(cj), nil
}

// AssembleToken joins a signing input and its raw signature into the wire token
// "signingInput.base64url(sig)". It performs no signing (no private key).
func AssembleToken(signingInput string, sig []byte) string {
	return signingInput + "." + b64.EncodeToString(sig)
}

// Verify checks the Ed25519 signature over the RECEIVED claims segment (it does
// not re-marshal), then decodes the claims and validates structure + the
// not_before/not_after window against now. It does NOT check replay.
func Verify(pub ed25519.PublicKey, token string, now time.Time) (Claims, error) {
	seg, sigPart, found := strings.Cut(token, ".")
	if !found || seg == "" || sigPart == "" {
		return Claims{}, ErrMalformedToken
	}
	sig, err := b64.DecodeString(sigPart)
	if err != nil {
		return Claims{}, ErrMalformedToken
	}
	if !ed25519.Verify(pub, []byte(seg), sig) {
		return Claims{}, ErrBadSignature
	}
	cj, err := b64.DecodeString(seg)
	if err != nil {
		return Claims{}, ErrMalformedToken
	}
	var c Claims
	if err := json.Unmarshal(cj, &c); err != nil {
		return Claims{}, ErrMalformedClaims
	}
	if c.JTI == "" || c.AssignmentID == "" || c.CID == "" || c.SourceNodeID == "" || c.DestNodeID == "" {
		return Claims{}, ErrMalformedClaims
	}
	if c.ProtocolVersion != ProtocolV1 || c.Generation <= 0 || c.MaxBytes <= 0 || c.NotBefore >= c.NotAfter {
		return Claims{}, ErrMalformedClaims
	}
	ts := now.Unix()
	if ts < c.NotBefore {
		return Claims{}, ErrNotYetValid
	}
	if ts > c.NotAfter {
		return Claims{}, ErrExpired
	}
	return c, nil
}

// EncodePublicKey renders an Ed25519 public key as base64url(raw 32 bytes) for
// delivery to donors via HeartbeatResponse.RepairTokenPublicKey (D-M4-7).
func EncodePublicKey(pub ed25519.PublicKey) string { return b64.EncodeToString(pub) }

// DecodePublicKey parses the wire form back into an Ed25519 public key.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := b64.DecodeString(s)
	if err != nil {
		return nil, ErrMalformedToken
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, ErrMalformedToken
	}
	return ed25519.PublicKey(raw), nil
}
