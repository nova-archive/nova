package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/localissuer"
	"github.com/nova-archive/nova/internal/auth/oidc"
	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/nova-archive/nova/internal/auth/token"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// signerSeedHex is a fixed 32-byte Ed25519 seed for the local issuer under test.
const signerSeedHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// TestIntegrationM6AuthThroughNginx exercises the M6 exit criteria end-to-end
// against the production (untagged) coordinator behind an nginx testcontainer:
// login → protected read → admin role matrix → expired access → refresh rotation
// → logout → authenticated upload owner_id → discovery doc.
func TestIntegrationM6AuthThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M6 integration test in short mode")
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

	backend := offlineBackend(t, ctx)

	signer, err := token.NewSignerFromSeed(signerSeedHex)
	require.NoError(t, err)
	const issuerURL = "https://nova.test/"
	iss, err := localissuer.New(localissuer.Config{
		Queries: gen.New(pool), Signer: signer, Gate: password.NewGate(4),
		IssuerURL: issuerURL, Audience: "nova",
		AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour,
	})
	require.NoError(t, err)

	authCfg := coordinator.AuthConfig{
		Verifiers:  []auth.Verifier{iss.Verifier()},
		Issuer:     iss,
		Descriptor: api.AuthConfigDescriptor{Mode: "local"},
		// PublicUploads left false → uploads require an uploader+ bearer.
		// LoginRate left zero → no per-IP login limiter (avoids test flakiness).
	}

	const coordPort = "19006"
	base := startCoordinatorWithNginx(t, ctx, pool, backend, ks, authCfg, coordPort, startNginxM6)

	// Seed users with argon2id password hashes.
	const pw = "hunter2hunter2"
	opUID := seedAuthUser(t, ctx, pool, "op@example.com", "operator", pw)
	_ = seedAuthUser(t, ctx, pool, "viewer@example.com", "viewer", pw)
	uploaderUID := seedAuthUser(t, ctx, pool, "uploader@example.com", "uploader", pw)

	// --- Floors (cheap re-assertions; full coverage lives in the unit suites) ---
	require.Error(t, auth.EnforceAnonymousPolicy(true), "T1.19: prod build refuses anonymous")

	// --- 1. login (operator) ---
	opAccess, opRefresh := m6Login(t, base, "op@example.com", pw)
	require.NotEmpty(t, opAccess)
	require.NotEmpty(t, opRefresh)

	// --- 2. GET /api/v1/users/me with bearer → 200, correct user ---
	{
		code, body := bearerGet(t, base+"/api/v1/users/me", opAccess)
		require.Equal(t, http.StatusOK, code)
		var u struct{ ID, Email, Role string }
		require.NoError(t, json.Unmarshal(body, &u))
		require.Equal(t, opUID.String(), u.ID)
		require.Equal(t, "operator", u.Role)
		require.Equal(t, "op@example.com", u.Email)
	}

	// --- 3. /users/me without a token → 401 ---
	{
		code, _ := bearerGet(t, base+"/api/v1/users/me", "")
		require.Equal(t, http.StatusUnauthorized, code)
	}

	// --- 4. admin boundary role matrix ---
	{
		// no token → 401
		code, _ := bearerGet(t, base+"/api/v1/admin/_probe", "")
		require.Equal(t, http.StatusUnauthorized, code)
		// operator → 404 (passed the guard; no admin endpoints until M7-M10)
		code, _ = bearerGet(t, base+"/api/v1/admin/_probe", opAccess)
		require.Equal(t, http.StatusNotFound, code)
		// viewer → 403 (insufficient role)
		viewerAccess, _ := m6Login(t, base, "viewer@example.com", pw)
		code, _ = bearerGet(t, base+"/api/v1/admin/_probe", viewerAccess)
		require.Equal(t, http.StatusForbidden, code)
	}

	// --- 5. expired access token → 401 ---
	{
		expired, err := signer.Sign(token.Mint{
			Subject: opUID.String(), Role: "operator", Issuer: issuerURL, Audience: "nova", TTL: -time.Minute,
		})
		require.NoError(t, err)
		code, _ := bearerGet(t, base+"/api/v1/users/me", expired)
		require.Equal(t, http.StatusUnauthorized, code)
	}

	// --- 6. refresh rotates; reusing the old refresh trips reuse detection ---
	var newRefresh string
	{
		code, body := postJSON(t, base+"/api/v1/auth/refresh", map[string]string{"refresh_token": opRefresh})
		require.Equal(t, http.StatusOK, code)
		var tr tokenResp
		require.NoError(t, json.Unmarshal(body, &tr))
		require.NotEmpty(t, tr.AccessToken)
		require.NotEqual(t, opRefresh, tr.RefreshToken)
		newRefresh = tr.RefreshToken

		// reuse of the now-rotated refresh → 401
		code, _ = postJSON(t, base+"/api/v1/auth/refresh", map[string]string{"refresh_token": opRefresh})
		require.Equal(t, http.StatusUnauthorized, code)
		// family revoked: the freshly-issued refresh is also dead now
		code, _ = postJSON(t, base+"/api/v1/auth/refresh", map[string]string{"refresh_token": newRefresh})
		require.Equal(t, http.StatusUnauthorized, code)
	}

	// --- 7. logout then refresh → 401 ---
	{
		// new session to logout cleanly
		_, ref := m6Login(t, base, "op@example.com", pw)
		code, _ := postJSON(t, base+"/api/v1/auth/logout", map[string]string{"refresh_token": ref})
		require.Equal(t, http.StatusNoContent, code)
		code, _ = postJSON(t, base+"/api/v1/auth/refresh", map[string]string{"refresh_token": ref})
		require.Equal(t, http.StatusUnauthorized, code)
	}

	// --- 8. upload policy + owner_id ---
	{
		// no token → 401 (default policy requires uploader+)
		body, ct := m6MultipartFile(t, []byte("hello world"), "text/plain")
		req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/blobs", body)
		req.Header.Set("Content-Type", ct)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

		// uploader token → 201 and owner_id set on the blob
		upAccess, _ := m6Login(t, base, "uploader@example.com", pw)
		body2, ct2 := m6MultipartFile(t, []byte("hello world owned"), "text/plain")
		req2, _ := http.NewRequest(http.MethodPost, base+"/api/v1/blobs", body2)
		req2.Header.Set("Content-Type", ct2)
		req2.Header.Set("Authorization", "Bearer "+upAccess)
		resp2, err := http.DefaultClient.Do(req2)
		require.NoError(t, err)
		rb, _ := io.ReadAll(resp2.Body)
		_ = resp2.Body.Close()
		require.Equal(t, http.StatusCreated, resp2.StatusCode, string(rb))
		var ur struct {
			CID string `json:"cid"`
		}
		require.NoError(t, json.Unmarshal(rb, &ur))
		require.Equal(t, uploaderUID.String(), blobOwner(t, ctx, pool, ur.CID))
	}

	// --- 9. discovery doc ---
	{
		code, body := bearerGet(t, base+"/api/v1/auth/config", "")
		require.Equal(t, http.StatusOK, code)
		var cfg struct{ Mode string }
		require.NoError(t, json.Unmarshal(body, &cfg))
		require.Equal(t, "local", cfg.Mode)
	}
}

// TestIntegrationM6ExternalOIDC boots the coordinator in external-OIDC mode
// against an in-process stub IdP and checks: local issuer endpoints 404; the
// discovery doc advertises the IdP; and an IdP-minted token verifies through
// the bearer middleware (admin role matrix).
func TestIntegrationM6ExternalOIDC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M6 external-OIDC integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)
	backend := offlineBackend(t, ctx)

	// Stub IdP: discovery + JWKS, Ed25519.
	idpPub, idpPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	const kid = "stub-kid"
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "jwks_uri": issuer + "/jwks",
			"authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token",
			"id_token_signing_alg_values_supported": []string{"EdDSA"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: idpPub, KeyID: kid, Algorithm: "EdDSA", Use: "sig"},
		}})
	})
	idp := httptest.NewServer(mux)
	defer idp.Close()
	issuer = idp.URL

	ver, err := oidc.New(ctx, oidc.Config{
		IssuerURL: issuer, ClientID: "nova",
		RoleClaim:   "groups",
		RoleMapping: map[string]string{"nova:operator": "operator", "nova:viewer": "viewer"},
	})
	require.NoError(t, err)

	authCfg := coordinator.AuthConfig{
		Verifiers:  []auth.Verifier{ver},
		Issuer:     nil, // external mode
		Descriptor: api.AuthConfigDescriptor{Mode: "external", IssuerURL: issuer, ClientID: "nova"},
	}

	c, err := coordinator.New(pool, backend, ks, coordinator.Config{
		ListenAddr: "127.0.0.1:0", Version: "m6-ext-itest",
		RateLimit: coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
		Auth:      authCfg,
	})
	require.NoError(t, err)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = c.Run(runCtx) }()
	require.Eventually(t, func() bool { return c.Addr() != "" }, 5*time.Second, 20*time.Millisecond)
	base := "http://" + c.Addr()

	// local issuer endpoints are disabled in external mode → 404 external_oidc_active
	{
		code, body := postJSON(t, base+"/api/v1/auth/login", map[string]string{"username": "x", "password": "y"})
		require.Equal(t, http.StatusNotFound, code)
		require.Contains(t, string(body), "external_oidc_active")
	}
	// discovery doc advertises the IdP
	{
		code, body := bearerGet(t, base+"/api/v1/auth/config", "")
		require.Equal(t, http.StatusOK, code)
		var cfg struct {
			Mode      string `json:"mode"`
			IssuerURL string `json:"issuer_url"`
		}
		require.NoError(t, json.Unmarshal(body, &cfg))
		require.Equal(t, "external", cfg.Mode)
		require.Equal(t, issuer, cfg.IssuerURL)
	}
	// an IdP-minted token verifies through the bearer middleware (admin matrix)
	mintExt := func(sub string, groups []string) string {
		sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: jose.JSONWebKey{Key: idpPriv, KeyID: kid}}, nil)
		require.NoError(t, err)
		raw, err := josejwt.Signed(sig).Claims(map[string]any{
			"iss": issuer, "sub": sub, "aud": "nova",
			"exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(),
			"groups": groups,
		}).Serialize()
		require.NoError(t, err)
		return raw
	}
	{
		// operator-mapped external token passes RequireRole → 404 (boundary)
		code, _ := bearerGet(t, base+"/api/v1/admin/_probe", mintExt("ext-op", []string{"nova:operator"}))
		require.Equal(t, http.StatusNotFound, code)
		// viewer-mapped external token → 403
		code, _ = bearerGet(t, base+"/api/v1/admin/_probe", mintExt("ext-viewer", []string{"nova:viewer"}))
		require.Equal(t, http.StatusForbidden, code)
		// no token → 401
		code, _ = bearerGet(t, base+"/api/v1/admin/_probe", "")
		require.Equal(t, http.StatusUnauthorized, code)
	}
}

// --- helpers ---

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	KID          string `json:"kid"`
}

func offlineBackend(t *testing.T, ctx context.Context) ipfs.Backend {
	t.Helper()
	swarm := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarm,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))
	backend, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath: t.TempDir(), Mode: ipfs.ModePrivate, SwarmKeyPath: swarm, Online: false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cc, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_ = backend.Close(cc)
	})
	return backend
}

// startCoordinatorWithNginx builds + runs a coordinator and fronts it with an
// nginx testcontainer via the provided helper, returning the public base URL.
func startCoordinatorWithNginx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, backend ipfs.Backend, ks *envelope.Keystore, authCfg coordinator.AuthConfig, coordPort string, nginx func(*testing.T, context.Context, string) string) string {
	t.Helper()
	c, err := coordinator.New(pool, backend, ks, coordinator.Config{
		ListenAddr:            "0.0.0.0:" + coordPort,
		Version:               "m6-itest",
		RateLimit:             coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
		MaxUploadSizeBytes:    4 << 20,
		MaxConcurrentAssembly: 4,
		SessionTTL:            time.Hour,
		UploadTmpDir:          t.TempDir(),
		UploadGCInterval:      time.Hour,
		Auth:                  authCfg,
	})
	require.NoError(t, err)
	runCtx, runCancel := context.WithCancel(ctx)
	t.Cleanup(runCancel)
	go func() { _ = c.Run(runCtx) }()
	require.Eventually(t, func() bool { return c.Addr() != "" }, 5*time.Second, 20*time.Millisecond)
	return nginx(t, ctx, coordPort)
}

func startNginxM6(t *testing.T, ctx context.Context, coordPort string) string {
	t.Helper()
	up := "http://host.testcontainers.internal:" + coordPort
	conf := fmt.Sprintf(`
server {
  listen 8080;
  location = /health          { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /blob/             { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/auth/      { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location = /api/v1/users/me { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/admin/     { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location = /api/v1/blobs    { proxy_pass %s; proxy_request_buffering off; proxy_set_header X-Forwarded-For $remote_addr; }
}
`, up, up, up, up, up, up)

	confPath := filepath.Join(t.TempDir(), "default.conf")
	require.NoError(t, ipfs.WriteFileForTest(confPath, []byte(conf)))

	req := testcontainers.ContainerRequest{
		Image:           "nginx:1.25-alpine",
		ExposedPorts:    []string{"8080/tcp"},
		HostAccessPorts: []int{atoiPort(t, coordPort)},
		WaitingFor:      wait.ForListeningPort("8080/tcp").WithStartupTimeout(60 * time.Second),
		Files: []testcontainers.ContainerFile{{
			HostFilePath:      confPath,
			ContainerFilePath: "/etc/nginx/conf.d/default.conf",
			FileMode:          0o644,
		}},
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cc, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_ = ctr.Terminate(cc)
	})
	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	mapped, err := ctr.MappedPort(ctx, "8080/tcp")
	require.NoError(t, err)
	return fmt.Sprintf("http://%s:%s", host, mapped.Port())
}

func seedAuthUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, email, role, pw string) uuid.UUID {
	t.Helper()
	hash, err := password.Hash(pw)
	require.NoError(t, err)
	u, err := gen.New(pool).CreateUser(ctx, gen.CreateUserParams{
		Email: email, Role: gen.UserRole(role), PasswordHash: pgtype.Text{String: hash, Valid: true},
	})
	require.NoError(t, err)
	return uuid.UUID(u.ID.Bytes)
}

func m6Login(t *testing.T, base, username, pw string) (access, refresh string) {
	t.Helper()
	code, body := postJSON(t, base+"/api/v1/auth/login", map[string]string{"username": username, "password": pw})
	require.Equal(t, http.StatusOK, code, string(body))
	var tr tokenResp
	require.NoError(t, json.Unmarshal(body, &tr))
	require.Equal(t, "bearer", tr.TokenType)
	return tr.AccessToken, tr.RefreshToken
}

func postJSON(t *testing.T, url string, payload any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func bearerGet(t *testing.T, url, access string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func m6MultipartFile(t *testing.T, content []byte, ctype string) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="f"`)
	hdr.Set("Content-Type", ctype)
	part, err := w.CreatePart(hdr)
	require.NoError(t, err)
	_, _ = part.Write(content)
	require.NoError(t, w.Close())
	return &b, w.FormDataContentType()
}

func blobOwner(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string) string {
	t.Helper()
	var owner pgtype.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT owner_id FROM blobs WHERE cid = $1`, cid).Scan(&owner))
	require.True(t, owner.Valid, "blob owner_id must be set")
	return uuid.UUID(owner.Bytes).String()
}
