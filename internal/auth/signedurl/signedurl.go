// Package signedurl implements Nova's signed-URL HMAC verifier, signer, and
// fail-through read-path Guard. The wire format and the six-step verification
// order are normative in docs/specs/SIGNED_URL_FORMAT.md; this package conforms
// to it exactly. Added in M7.
package signedurl

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Failure codes returned in the 403 error body's `code` field. Each maps to one
// verification step (SIGNED_URL_FORMAT.md § "Verification").
const (
	CodeMissingParam = "signature_missing_param"
	CodeUnknownKID   = "signature_unknown_kid"
	CodeRevoked      = "signature_revoked"
	CodeExpired      = "signature_expired"
	CodeInvalid      = "signature_invalid"
	CodeAudMismatch  = "signature_aud_mismatch"
)

// ErrUnknownKID is returned by KeySource.ByKID when no usable signing key
// (state active, or retired within its grace window) exists for the kid.
var ErrUnknownKID = errors.New("signedurl: no usable signing key for kid")

// Key is an unwrapped signing key resolved by kid.
type Key struct {
	KID    string
	Secret []byte
}

// KeySource resolves signing keys by kid for verification and minting.
type KeySource interface {
	ByKID(ctx context.Context, kid string) (Key, error)
}

// Revocations reports whether any parsed field of a signed URL is revoked.
type Revocations interface {
	IsRevoked(cid, aud, kid, path string) bool
}

// Canonical builds the HMAC input string: path, exp, aud, kid joined by single
// LF bytes with no trailing newline (SIGNED_URL_FORMAT.md § "Canonical string").
func Canonical(path string, exp int64, aud, kid string) string {
	return path + "\n" + strconv.FormatInt(exp, 10) + "\n" + aud + "\n" + kid
}

// Sign returns base64url(HMAC-SHA256(key, canonical)) without padding.
func Sign(key []byte, canonical string) string {
	return base64.RawURLEncoding.EncodeToString(hmacSum(key, canonical))
}

func hmacSum(key []byte, canonical string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(canonical))
	return m.Sum(nil)
}

// dummyKey equalises the HMAC work on the unknown-kid path so response timing
// does not reveal whether a kid resolves to a real key.
var dummyKey = make([]byte, 32)

// VerifyInput is the request material the verifier needs.
type VerifyInput struct {
	Path    string
	Query   url.Values
	Origin  string
	Referer string
	Now     time.Time
}

// Decision is the verifier's verdict. On failure Code is one of the signature_*
// constants; on success the parsed fields are populated for logging.
type Decision struct {
	OK            bool
	Code          string
	CID, Aud, Kid string
}

func deny(code string) Decision { return Decision{Code: code} }

// Verifier runs the six-step verification against a KeySource and Revocations.
type Verifier struct {
	keys KeySource
	revs Revocations
}

// NewVerifier builds a Verifier.
func NewVerifier(keys KeySource, revs Revocations) *Verifier {
	return &Verifier{keys: keys, revs: revs}
}

// Verify performs the six-step check in spec order (schema, kid, revocation,
// expiry, signature, audience). Any failure short-circuits to a Decision with
// OK=false and the step's code. The HMAC compare is constant-time and is always
// performed (against dummyKey on an unknown kid) so failure timing does not
// distinguish the failed step. Clock-skew tolerance is 0 s.
func (v *Verifier) Verify(ctx context.Context, in VerifyInput) Decision {
	// 1. Schema: all four params present and well-formed.
	sigStr := in.Query.Get("sig")
	expStr := in.Query.Get("exp")
	aud := in.Query.Get("aud")
	kid := in.Query.Get("kid")
	if sigStr == "" || expStr == "" || aud == "" || kid == "" {
		return deny(CodeMissingParam)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil {
		return deny(CodeMissingParam)
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return deny(CodeMissingParam)
	}

	cid := cidFromPath(in.Path)
	canonical := Canonical(in.Path, exp, aud, kid)

	// Past the schema stage the parsed fields are known; carry them on every
	// outcome so the Guard can log kid/aud/cid for both grants and rejections.
	fail := func(code string) Decision {
		return Decision{Code: code, CID: cid, Aud: aud, Kid: kid}
	}

	// 2. Key lookup. On a miss use the dummy key so the HMAC compare below runs
	//    with identical cost (any lookup error is treated as unknown kid; a
	//    transient DB error therefore surfaces as a 403, a Phase-1 simplification
	//    acceptable because the verifier also caches keys).
	key, kerr := v.keys.ByKID(ctx, kid)
	secret := key.Secret
	if kerr != nil {
		secret = dummyKey
	}

	// 5 (computed up front, constant-time): recompute and compare the signature.
	sigOK := subtle.ConstantTimeCompare(hmacSum(secret, canonical), sig) == 1

	if kerr != nil {
		return fail(CodeUnknownKID)
	}
	// 3. Revocation.
	if v.revs.IsRevoked(cid, aud, kid, in.Path) {
		return fail(CodeRevoked)
	}
	// 4. Expiry — strictly greater than now (0 s skew).
	if exp <= in.Now.Unix() {
		return fail(CodeExpired)
	}
	// 5. Signature.
	if !sigOK {
		return fail(CodeInvalid)
	}
	// 6. Audience.
	if !audienceMatches(in.Origin, in.Referer, aud) {
		return fail(CodeAudMismatch)
	}
	return Decision{OK: true, CID: cid, Aud: aud, Kid: kid}
}

// HasParams reports whether the query carries any signed-URL parameter. The
// Guard uses it to decide whether a request is claiming signed-URL authorization
// (so a request with none passes straight through).
func HasParams(q url.Values) bool {
	return q.Has("sig") || q.Has("exp") || q.Has("aud") || q.Has("kid")
}

// cidFromPath extracts the leading content id from /blob/{cid}[...] or
// /i/{cid}/... — the segment up to the first dot (a CID contains no dot), so
// /blob/{cid}.json and /i/{cid}.webp resolve to the bare cid.
func cidFromPath(path string) string {
	p := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	switch parts[0] {
	case "blob", "i":
		seg := parts[1]
		if i := strings.IndexByte(seg, '.'); i >= 0 {
			seg = seg[:i]
		}
		return seg
	}
	return ""
}

// audienceMatches parses Origin (else Referer) down to scheme://host[:port] and
// compares it byte-for-byte to aud.
func audienceMatches(origin, referer, aud string) bool {
	src := origin
	if src == "" {
		src = referer
	}
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return u.Scheme+"://"+u.Host == aud
}
