package auth

import (
	"context"
	"errors"
)

// Identity is the authenticated caller, hydrated by the bearer middleware from
// a verified token. The zero value means "no identity" (anonymous).
type Identity struct {
	UserID string // token subject (uuid for local issuer)
	Role   string // viewer|uploader|moderator|operator
	Issuer string // token iss
}

// Verifier validates a raw bearer token and returns the caller's Identity.
type Verifier interface {
	// Verify returns ErrTokenNotForMe (sentinel) when the token's issuer is not
	// this verifier's, so the middleware tries the next verifier without
	// treating it as a hard failure. It returns ErrAuthUnavailable when the
	// verifier cannot currently reach its key source (⇒ retryable 503). Any
	// other error means the token is invalid for this verifier (⇒ 401).
	Verify(ctx context.Context, raw string) (Identity, error)
}

// ErrTokenNotForMe signals the token's issuer does not match this verifier.
var ErrTokenNotForMe = errors.New("auth: token not issued by this verifier")

// ErrAuthUnavailable signals the verifier's key source is temporarily
// unreachable (external IdP discovery pending). Callers map this to 503.
var ErrAuthUnavailable = errors.New("auth: verification temporarily unavailable")

type identityCtxKey struct{}

// ContextWithIdentity returns ctx carrying id.
func ContextWithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext returns the identity hydrated by the bearer middleware.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}
