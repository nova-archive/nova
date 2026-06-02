package storage

import "context"

// readAuthzKey marks a request context as authorized to read otherwise-private
// content. It is set only by the signed-URL Guard after a successful,
// path-bound signature verification — the HMAC binds the grant to the exact
// request path, so it can never authorize a different CID. The grant is
// request-scoped (never stored, never crosses requests).
type readAuthzKey struct{}

// WithReadAuthz returns a context that authorizes Resolve to serve private
// blobs for this request. Used by internal/auth/signedurl's Guard.
func WithReadAuthz(ctx context.Context) context.Context {
	return context.WithValue(ctx, readAuthzKey{}, true)
}

// readAuthorized reports whether ctx carries a signed-URL read grant.
func readAuthorized(ctx context.Context) bool {
	v, _ := ctx.Value(readAuthzKey{}).(bool)
	return v
}
