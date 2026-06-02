package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/localissuer"
	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/auth/token"
	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
)

// TestIntegrationM7SignedURLsThroughNginx exercises the M7 exit criteria
// end-to-end behind nginx: a private blob is unreachable without a signature;
// an operator-minted signed URL serves it; wrong-origin / tampered / expired
// signatures 403; a moderator can revoke (and cannot rotate); a revoked CID
// 403s; and a public blob is unaffected by the Guard.
func TestIntegrationM7SignedURLsThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M7 integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)
	require.NoError(t, signedurl.EnsureActiveKey(ctx, gen.New(pool), ks))

	backend := offlineBackend(t, ctx)

	signer, err := token.NewSignerFromSeed(signerSeedHex)
	require.NoError(t, err)
	iss, err := localissuer.New(localissuer.Config{
		Queries: gen.New(pool), Signer: signer, Gate: password.NewGate(4),
		IssuerURL: "https://nova.test/", Audience: "nova",
		AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour,
	})
	require.NoError(t, err)
	authCfg := coordinator.AuthConfig{
		Verifiers:  []auth.Verifier{iss.Verifier()},
		Issuer:     iss,
		Descriptor: api.AuthConfigDescriptor{Mode: "local"},
	}

	const coordPort = "19007"
	base := startCoordinatorWithNginx(t, ctx, pool, backend, ks, authCfg, coordPort, startNginxM6)

	const pw = "hunter2hunter2"
	_ = seedAuthUser(t, ctx, pool, "op@example.com", "operator", pw)
	_ = seedAuthUser(t, ctx, pool, "mod@example.com", "moderator", pw)

	priv, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("top secret"), MIME: "text/plain", Visibility: "private"})
	require.NoError(t, err)
	pub, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("hello world"), MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)

	// 1. Baseline: private blob unreachable without a signature; public blob
	//    serves anonymously (Guard passes through).
	requireStatus(t, base+"/blob/"+priv.CID, http.StatusUnauthorized)
	requireStatus(t, base+"/blob/"+pub.CID, http.StatusOK)

	// 2. Operator mints a signed URL for the private blob.
	opTok, _ := m6Login(t, base, "op@example.com", pw)
	const aud = "http://embed.test"
	code, body := doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/signed-urls/sign", opTok,
		map[string]any{"path": "/blob/" + priv.CID, "ttl_seconds": 300, "aud": aud})
	require.Equal(t, http.StatusCreated, code, string(body))
	var signed struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(body, &signed))
	require.Contains(t, signed.URL, "/blob/"+priv.CID)

	// 3. The signed URL with a matching Origin serves the private bytes.
	st, got := getWithOrigin(t, base+signed.URL, aud)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "top secret", string(got))

	// 4. Wrong Origin and tampered signature both 403.
	st, _ = getWithOrigin(t, base+signed.URL, "http://evil.test")
	require.Equal(t, http.StatusForbidden, st, "aud mismatch")
	st, tb := getWithOrigin(t, base+tamperSig(t, signed.URL), aud)
	require.Equal(t, http.StatusForbidden, st)
	require.Contains(t, string(tb), signedurl.CodeInvalid)

	// 5. Expiry: a 1-second URL is rejected after it lapses.
	code, body = doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/signed-urls/sign", opTok,
		map[string]any{"path": "/blob/" + priv.CID, "ttl_seconds": 1, "aud": aud})
	require.Equal(t, http.StatusCreated, code, string(body))
	var shortLived struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(body, &shortLived))
	time.Sleep(2 * time.Second)
	st, eb := getWithOrigin(t, base+shortLived.URL, aud)
	require.Equal(t, http.StatusForbidden, st)
	require.Contains(t, string(eb), signedurl.CodeExpired)

	// 6. Operator can rotate the signing key.
	code, _ = doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/keys/rotate-signing", opTok, map[string]any{})
	require.Equal(t, http.StatusCreated, code)

	// 7. Authz: a moderator cannot rotate (operator-only) but can revoke.
	modTok, _ := m6Login(t, base, "mod@example.com", pw)
	code, _ = doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/keys/rotate-signing", modTok, map[string]any{})
	require.Equal(t, http.StatusForbidden, code, "rotate is operator-only")
	code, _ = doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/signed-urls/revoke", modTok,
		map[string]any{"kind": "cid", "value": priv.CID})
	require.Equal(t, http.StatusCreated, code)

	// 8. The original signed URL is now revoked (still within the old key's
	//    grace window, so this proves CID revocation, not key expiry).
	st, rb := getWithOrigin(t, base+signed.URL, aud)
	require.Equal(t, http.StatusForbidden, st)
	require.Contains(t, string(rb), signedurl.CodeRevoked)

	// 9. The admin surface rejects an unauthenticated mint.
	code, _ = doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/signed-urls/sign", "",
		map[string]any{"path": "/blob/" + pub.CID, "ttl_seconds": 60, "aud": aud})
	require.Equal(t, http.StatusUnauthorized, code)
}

func doJSONAuth(t *testing.T, method, url, token string, payload any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		require.NoError(t, err)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func getWithOrigin(t *testing.T, url, origin string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// tamperSig flips one character of the sig query parameter.
func tamperSig(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	q := u.Query()
	s := []byte(q.Get("sig"))
	require.NotEmpty(t, s)
	if s[0] == 'A' {
		s[0] = 'B'
	} else {
		s[0] = 'A'
	}
	q.Set("sig", string(s))
	u.RawQuery = q.Encode()
	return u.String()
}
