package oidc

// White-box test for the resilient-discovery (retry) path. Uses package oidc
// so it can call newWithBackoff directly, setting a short initialBackoff to
// avoid the production 1s sleep.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/stretchr/testify/require"
)

// TestRetryRecoveryUsesCorrectAudience exercises the retry/recovery path:
//  1. Discovery handler initially returns 503 → New returns (v, nil) with ready=false.
//  2. Verify returns ErrAuthUnavailable while the IdP is down.
//  3. Discovery is unblocked; retryDiscovery eventually succeeds.
//  4. A valid token (aud == ClientID) now verifies correctly → catches I1.
func TestRetryRecoveryUsesCorrectAudience(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	kid := "retry-kid"

	// Atomic flag: 0 = return 503, 1 = serve normally.
	var discoveryOK atomic.Int32

	mux := http.NewServeMux()
	var issuer string // set after server starts

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if discoveryOK.Load() == 0 {
			http.Error(w, "IdP not ready", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"jwks_uri":                              issuer + "/jwks",
			"authorization_endpoint":                issuer + "/auth",
			"token_endpoint":                        issuer + "/token",
			"id_token_signing_alg_values_supported": []string{"EdDSA"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: pub, KeyID: kid, Algorithm: "EdDSA", Use: "sig"},
		}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL

	cfg := Config{
		IssuerURL:   issuer,
		ClientID:    "nova",
		RoleClaim:   "groups",
		RoleMapping: map[string]string{"nova:operator": "operator"},
	}

	// Use 10ms initial backoff so the retry fires quickly in tests.
	ctx := context.Background()
	v, err := newWithBackoff(ctx, cfg, 10*time.Millisecond)
	require.NoError(t, err, "New must not error on discovery failure (resilient)")
	require.NotNil(t, v)

	// --- Phase 1: Verifier should be unavailable while discovery is blocked ---
	_, verifyErr := v.Verify(ctx, "dummy")
	require.ErrorIs(t, verifyErr, auth.ErrAuthUnavailable,
		"expected ErrAuthUnavailable while IdP is down")

	// --- Phase 2: Unblock discovery and wait for retry to recover ---
	discoveryOK.Store(1)

	// Build a valid token to use once the verifier has recovered.
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.EdDSA, Key: jose.JSONWebKey{Key: priv, KeyID: kid}},
		nil,
	)
	require.NoError(t, err)

	raw, err := josejwt.Signed(signer).Claims(map[string]any{
		"iss":    issuer,
		"sub":    "retry-user",
		"aud":    "nova", // aud == ClientID, NOT IssuerURL — catches I1
		"exp":    time.Now().Add(time.Minute).Unix(),
		"iat":    time.Now().Unix(),
		"groups": []string{"nova:operator"},
	}).Serialize()
	require.NoError(t, err)

	// Poll until the retry goroutine has succeeded and the token verifies.
	require.Eventually(t, func() bool {
		id, err := v.Verify(ctx, raw)
		if err != nil {
			return false
		}
		// Also assert the identity is correctly populated.
		require.Equal(t, "retry-user", id.UserID)
		require.Equal(t, "operator", id.Role)
		return true
	}, 3*time.Second, 20*time.Millisecond,
		"verifier did not recover within 3s after discovery became available")
}
