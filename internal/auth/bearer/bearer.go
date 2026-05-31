// Package bearer provides chi-compatible HTTP middleware for bearer-token
// authentication. Optional hydrates the request context with a verified
// Identity (if any token is present and verifiable); RequireAuthenticated and
// RequireRole enforce access control without doing any verification themselves.
package bearer

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auth"
)

// authStateKey is the private context key used to stash token-resolution state
// so that guard middleware can distinguish "no token" from "IdP unavailable".
type authStateKey struct{}

// state carries the result of the Optional middleware's verification attempt.
type state struct {
	id          auth.Identity
	haveID      bool
	unavailable bool
}

// stateFromContext returns the state stashed by Optional, or a zero state if
// Optional was not in the middleware chain (or if there was no bearer token).
func stateFromContext(r *http.Request) state {
	if s, ok := r.Context().Value(authStateKey{}).(state); ok {
		return s
	}
	return state{}
}

// extractBearer returns the raw token from an "Authorization: Bearer <token>"
// header, accepting the scheme case-insensitively. Returns "" if the header is
// absent or does not carry a bearer token.
func extractBearer(r *http.Request) string {
	hdr := r.Header.Get("Authorization")
	if hdr == "" {
		return ""
	}
	scheme, rest, ok := strings.Cut(hdr, " ")
	if !ok {
		return ""
	}
	if !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

// Optional iterates vs, attempts to verify the bearer token (if any), and
// stores the result in the request context. It NEVER rejects a request — all
// rejection happens in the guard middleware below.
func Optional(vs []auth.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := extractBearer(r)
			if raw == "" {
				// No bearer token — proceed with no state stashed.
				next.ServeHTTP(w, r)
				return
			}

			var st state
			for _, v := range vs {
				id, err := v.Verify(r.Context(), raw)
				if err == nil {
					st.id = id
					st.haveID = true
					break
				}
				if errors.Is(err, auth.ErrTokenNotForMe) {
					continue
				}
				if errors.Is(err, auth.ErrAuthUnavailable) {
					st.unavailable = true
					continue
				}
				// Any other error: token invalid for this verifier — try next.
			}

			ctx := r.Context()
			if st.haveID {
				ctx = auth.ContextWithIdentity(ctx, st.id)
			}
			// Always stash state so guards can read unavailable flag.
			ctx = context.WithValue(ctx, authStateKey{}, st)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAuthenticated is a guard handler (not a middleware factory) that
// passes requests through only when the context carries a verified Identity.
func RequireAuthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.IdentityFromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		writeAuthFailure(w, r, stateFromContext(r))
	})
}

// RequireRole is a guard middleware factory. It passes requests through only
// when the Identity's Role is one of the allowed roles. If no identity is
// present it falls back to the same 503/401 logic as RequireAuthenticated.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		allowed[role] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			id, ok := auth.IdentityFromContext(ctx)
			if !ok {
				writeAuthFailure(w, r, stateFromContext(r))
				return
			}
			if _, permitted := allowed[id.Role]; !permitted {
				httputil.WriteError(w, http.StatusForbidden, "forbidden",
					"you do not have the required role", middleware.RequestIDFromContext(ctx))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeAuthFailure writes the appropriate 503 or 401 response depending on
// whether the IdP was reachable.
func writeAuthFailure(w http.ResponseWriter, r *http.Request, st state) {
	ctx := r.Context()
	reqID := middleware.RequestIDFromContext(ctx)
	if st.unavailable {
		w.Header().Set("Retry-After", "2")
		httputil.WriteError(w, http.StatusServiceUnavailable, "auth_unavailable",
			"authentication service temporarily unavailable", reqID)
		return
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	httputil.WriteError(w, http.StatusUnauthorized, "unauthenticated",
		"authentication required", reqID)
}
