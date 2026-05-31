package oidc_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/oidc"
	"github.com/stretchr/testify/require"
)

// stubIDP creates a test OIDC identity provider and returns:
//   - the issuer URL string
//   - a go-jose Signer using the IdP's private key
//   - a configured *oidc.Verifier pointing at that IdP
func stubIDP(t *testing.T) (issuerURL string, sig jose.Signer, v *oidc.Verifier) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	kid := "test-kid"

	mux := http.NewServeMux()
	var issuer string // filled in after server starts

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
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

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.EdDSA, Key: jose.JSONWebKey{Key: priv, KeyID: kid}},
		nil,
	)
	require.NoError(t, err)

	ctx := context.Background()
	verifier, err := oidc.New(ctx, oidc.Config{
		IssuerURL:   issuer,
		ClientID:    "nova",
		RoleClaim:   "groups",
		RoleMapping: map[string]string{"nova:operator": "operator"},
	})
	require.NoError(t, err)

	return issuer, signer, verifier
}

func TestExternalVerifyMapsClaimToRole(t *testing.T) {
	ctx := context.Background()
	issuer, sig, v := stubIDP(t)

	raw, err := jwt.Signed(sig).Claims(map[string]any{
		"iss": issuer, "sub": "ext-user", "aud": "nova",
		"exp":    time.Now().Add(time.Minute).Unix(),
		"iat":    time.Now().Unix(),
		"groups": []string{"nova:operator"},
	}).Serialize()
	require.NoError(t, err)

	id, err := v.Verify(ctx, raw)
	require.NoError(t, err)
	require.Equal(t, "ext-user", id.UserID)
	require.Equal(t, "operator", id.Role)
}

func TestForeignIssuerTokenNotForMe(t *testing.T) {
	ctx := context.Background()
	// Build an IdP but use a token with a DIFFERENT issuer.
	_, sig, v := stubIDP(t)

	// Mint a token signed with the stub key but claiming iss = "https://other/"
	// so the signature is technically valid but the issuer is wrong.
	raw, err := jwt.Signed(sig).Claims(map[string]any{
		"iss": "https://other/",
		"sub": "foreign-user",
		"aud": "nova",
		"exp": time.Now().Add(time.Minute).Unix(),
		"iat": time.Now().Unix(),
	}).Serialize()
	require.NoError(t, err)

	_, err = v.Verify(ctx, raw)
	require.Error(t, err)
	require.True(t, errors.Is(err, auth.ErrTokenNotForMe),
		"expected ErrTokenNotForMe, got: %v", err)
}
