package signedurl

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidPath is returned by Mint when path is not a readable content path
// (/blob/{cid} or /i/{cid}/...).
var ErrInvalidPath = errors.New("signedurl: path is not a readable content path")

// activeSource resolves the current active signing key (for minting).
type activeSource interface {
	Active(ctx context.Context) (Key, error)
}

// MintResult is a freshly-signed URL and its components.
type MintResult struct {
	URL string
	KID string
	Exp int64
}

// Mint builds a signed URL for path, valid for ttl from now, bound to aud. The
// caller is responsible for capping ttl. Returns ErrInvalidPath for a
// non-content path.
func Mint(ctx context.Context, src activeSource, path string, ttl time.Duration, aud string) (MintResult, error) {
	if !IsContentPath(path) {
		return MintResult{}, ErrInvalidPath
	}
	key, err := src.Active(ctx)
	if err != nil {
		return MintResult{}, err
	}
	exp := time.Now().Add(ttl).Unix()
	sig := Sign(key.Secret, Canonical(path, exp, aud, key.KID))

	q := url.Values{}
	q.Set("sig", sig)
	q.Set("exp", strconv.FormatInt(exp, 10))
	q.Set("aud", aud)
	q.Set("kid", key.KID)
	return MintResult{URL: path + "?" + q.Encode(), KID: key.KID, Exp: exp}, nil
}

// IsContentPath reports whether path addresses readable blob/image content
// (and therefore is a valid target for a signed URL).
func IsContentPath(path string) bool {
	if rest, ok := strings.CutPrefix(path, "/blob/"); ok {
		return rest != ""
	}
	if rest, ok := strings.CutPrefix(path, "/i/"); ok {
		return rest != ""
	}
	return false
}
