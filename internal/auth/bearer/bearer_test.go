package bearer_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/bearer"
	"github.com/stretchr/testify/require"
)

type fakeVerifier struct {
	id  auth.Identity
	err error
}

func (f fakeVerifier) Verify(_ context.Context, _ string) (auth.Identity, error) { return f.id, f.err }

func ok200(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }

func serve(t *testing.T, mw func(http.Handler) http.Handler, authz string, final http.HandlerFunc) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	rr := httptest.NewRecorder()
	mw(http.HandlerFunc(final)).ServeHTTP(rr, r)
	return rr.Code
}

func TestRequireAuthenticated(t *testing.T) {
	t.Parallel()
	good := []auth.Verifier{fakeVerifier{id: auth.Identity{UserID: "u", Role: "viewer"}}}
	notme := []auth.Verifier{fakeVerifier{err: auth.ErrTokenNotForMe}}
	down := []auth.Verifier{fakeVerifier{err: auth.ErrAuthUnavailable}}

	chain := func(vs []auth.Verifier) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler { return bearer.Optional(vs)(bearer.RequireAuthenticated(next)) }
	}
	require.Equal(t, 200, serve(t, chain(good), "Bearer t", ok200))
	require.Equal(t, 401, serve(t, chain(good), "", ok200))          // no token
	require.Equal(t, 401, serve(t, chain(notme), "Bearer t", ok200)) // unverifiable
	require.Equal(t, 503, serve(t, chain(down), "Bearer t", ok200))  // IdP discovery pending
}

func TestRequireRole(t *testing.T) {
	t.Parallel()
	op := []auth.Verifier{fakeVerifier{id: auth.Identity{UserID: "u", Role: "operator"}}}
	viewer := []auth.Verifier{fakeVerifier{id: auth.Identity{UserID: "u", Role: "viewer"}}}
	chain := func(vs []auth.Verifier) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return bearer.Optional(vs)(bearer.RequireRole("operator", "moderator")(next))
		}
	}
	require.Equal(t, 200, serve(t, chain(op), "Bearer t", ok200))
	require.Equal(t, 403, serve(t, chain(viewer), "Bearer t", ok200))
	require.Equal(t, 401, serve(t, chain(viewer), "", ok200))
}

func TestRequireRoleUnavailable(t *testing.T) {
	t.Parallel()
	down := []auth.Verifier{fakeVerifier{err: auth.ErrAuthUnavailable}}
	chain := func(vs []auth.Verifier) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return bearer.Optional(vs)(bearer.RequireRole("operator")(next))
		}
	}
	// IdP down + bearer token present → 503, not 401
	require.Equal(t, 503, serve(t, chain(down), "Bearer t", ok200))
}

var _ = errors.Is
