package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
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
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestIntegrationM10MasterKeyRotationThroughNginx is the M10 exit criterion:
// it proves that a master-key rotation re-wraps all DEKs and signing keys from
// v1 to v2 (online, through the live HTTP API behind nginx), respects legal
// holds, retires v1, and keeps blob reads + signed-URL serving working
// throughout and after the rotation.
func TestIntegrationM10MasterKeyRotationThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M10 integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// --- Shared pool + backend (both phases share these) ---
	pool := dbtest.New(t, ctx)
	backend := offlineBackend(t, ctx)

	// =========================================================================
	// Phase 1 — seed v1-wrapped state (no HTTP needed)
	// =========================================================================

	t.Setenv("NOVA_MASTER_KEY_V1", "aabbccddeeff00112233445566778899aabbccddeeff001122334455667788ff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")

	ks1, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks1.Bootstrap(ctx)
	require.NoError(t, err)
	require.NoError(t, signedurl.EnsureActiveKey(ctx, gen.New(pool), ks1))

	// Seed 3 blobs under v1:
	//   encBlob  — encrypted, public; will be read before + after rotation.
	//   holdBlob — encrypted, public; its DEK is put under legal hold.
	//   pubBlob  — public_archival (unencrypted, no DEK); immune to rotation.
	encBlob, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks1},
		blobfixture.Spec{Plaintext: []byte("rotate me"), MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)

	holdBlob, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks1},
		blobfixture.Spec{Plaintext: []byte("hold me"), MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)

	_, err = blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks1},
		blobfixture.Spec{Plaintext: []byte("public bytes"), MIME: "text/plain", Visibility: "public", Unencrypted: true})
	require.NoError(t, err)

	// Put holdBlob's DEK under legal hold.
	_, err = pool.Exec(ctx,
		`UPDATE data_encryption_keys SET legal_hold = true
		 WHERE id = (SELECT encryption_key_id FROM blobs WHERE cid = $1)`,
		holdBlob.CID)
	require.NoError(t, err)

	// =========================================================================
	// Phase 2 — bring up coordinator with {v1,v2} loaded, active=v2, and rotate
	// =========================================================================

	// Introduce v2 as a NEW distinct key (different bytes from v1).
	t.Setenv("NOVA_MASTER_KEY_V2", "1122334455667788ff00aabbccddeeff1122334455667788ff00aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v2")
	// V1 is still set in the environment (from Phase 1 t.Setenv above).

	ks2, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	// Bootstrap inserts v2 'active'; v1 row already exists in the DB.
	_, err = ks2.Bootstrap(ctx)
	require.NoError(t, err)

	// Auth wiring (verbatim copy of M9 pattern).
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

	const coordPort = "19010"
	base := startCoordinatorWithNginxCfg(t, ctx, pool, backend, ks2, coordinator.Config{
		ListenAddr:            "0.0.0.0:" + coordPort,
		Version:               "m10-itest",
		RateLimit:             coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
		MaxUploadSizeBytes:    4 << 20,
		MaxConcurrentAssembly: 4,
		SessionTTL:            time.Hour,
		UploadTmpDir:          t.TempDir(),
		UploadGCInterval:      time.Hour,
		Auth:                  authCfg,
		MasterKeyRotation: coordinator.MasterKeyRotationConfig{
			RewrapConcurrency: 2,
			RewrapBatchSize:   4,
			RewrapPace:        0, // no inter-batch sleep for speed
		},
	}, startNginxM10)

	// Seed operator + moderator users.
	const pw = "hunter2hunter2"
	_ = seedAuthUser(t, ctx, pool, "op10@example.com", "operator", pw)
	_ = seedAuthUser(t, ctx, pool, "mod10@example.com", "moderator", pw)
	opTok, _ := m6Login(t, base, "op10@example.com", pw)
	modTok, _ := m6Login(t, base, "mod10@example.com", pw)

	// --- Pre-rotation reads ---
	// encBlob (public, v1-encrypted) must be readable before rotation.
	rc, preBody := doJSONAuth(t, http.MethodGet, base+"/blob/"+encBlob.CID, "", nil)
	require.Equal(t, http.StatusOK, rc, "pre-rotation read of encBlob must succeed")
	require.Equal(t, "rotate me", string(preBody))

	// Mint a signed URL under v1 (the signing key is still wrapped under v1 now).
	sCode, sBody := doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/signed-urls/sign", opTok,
		map[string]any{
			"path":        "/blob/" + encBlob.CID,
			"ttl_seconds": 3600,
			"aud":         "https://embed.test",
		})
	require.Equal(t, http.StatusCreated, sCode, string(sBody))
	var signedURLResp struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(sBody, &signedURLResp))
	require.NotEmpty(t, signedURLResp.URL)
	signedURL := signedURLResp.URL

	// Verify the signed URL works pre-rotation (baseline).
	preSigned, _ := getWithOrigin(t, base+signedURL, "https://embed.test")
	require.Equal(t, http.StatusOK, preSigned, "signed URL must work before rotation")

	// --- Trigger rotation: v1 → v2 ---
	rotCode, rotBody := doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/keys/rotate-master", opTok,
		map[string]any{"from_version": "v1", "to_version": "v2"})
	require.Equal(t, http.StatusAccepted, rotCode, string(rotBody))

	// --- Poll rotation-status until in_progress is null (rotation complete) ---
	type rotationStatusResp struct {
		Active     string `json:"active"`
		InProgress *struct {
			From          string `json:"from"`
			RemainingDEKs int64  `json:"remaining_deks"`
			Stalled       bool   `json:"stalled"`
		} `json:"in_progress"`
		Versions []struct {
			Label string `json:"label"`
			State string `json:"state"`
		} `json:"versions"`
	}

	const maxPolls = 150
	const pollInterval = 200 * time.Millisecond
	done := false
	for i := 0; i < maxPolls; i++ {
		stCode, stBody := doJSONAuth(t, http.MethodGet, base+"/api/v1/admin/keys/rotation-status", opTok, nil)
		require.Equal(t, http.StatusOK, stCode, string(stBody))
		var st rotationStatusResp
		require.NoError(t, json.Unmarshal(stBody, &st))
		if st.InProgress == nil {
			done = true
			break
		}
		require.False(t, st.InProgress.Stalled, "rotation must not be stalled")
		time.Sleep(pollInterval)
	}
	require.True(t, done, "rotation did not complete within %v", time.Duration(maxPolls)*pollInterval)

	// --- Assert exit criteria via direct DB queries ---

	// 1. v1 row ID.
	var v1id pgtype.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM master_key_versions WHERE version_label = 'v1'`).Scan(&v1id))

	// 2. No active/rotating DEKs remain under v1.
	m10AssertCountZero(t, ctx, pool,
		`SELECT count(*) FROM data_encryption_keys
		 WHERE master_key_version_id = $1 AND state IN ('active','rotating')`,
		v1id, "v1 DEKs with state active/rotating must be 0 after rotation")

	// 3. No active/retired signing keys remain under v1.
	m10AssertCountZero(t, ctx, pool,
		`SELECT count(*) FROM signing_keys
		 WHERE master_key_version_id = $1 AND state IN ('active','retired')`,
		v1id, "v1 signing keys with state active/retired must be 0 after rotation")

	// 4. v1 state must be 'retired'.
	var v1State string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT state FROM master_key_versions WHERE version_label = 'v1'`).Scan(&v1State))
	require.Equal(t, "retired", v1State, "v1 must be retired after rotation")

	// 5. Legal-hold DEK was re-wrapped (not shredded): legal_hold=true, state='active', version=v2.
	var v2id pgtype.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM master_key_versions WHERE version_label = 'v2'`).Scan(&v2id))

	var holdLegal bool
	var holdState string
	var holdVersionID pgtype.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT k.legal_hold, k.state, k.master_key_version_id
		 FROM data_encryption_keys k
		 JOIN blobs b ON b.encryption_key_id = k.id
		 WHERE b.cid = $1`, holdBlob.CID).Scan(&holdLegal, &holdState, &holdVersionID))
	require.True(t, holdLegal, "legal_hold must remain true after rotation")
	require.Equal(t, "active", holdState, "legal-hold DEK must be active (not shredded) after rotation")
	require.Equal(t, v2id, holdVersionID, "legal-hold DEK must be re-wrapped under v2")

	// --- Post-rotation reads ---

	// encBlob (now v2-encrypted) must still be readable.
	postRC, postBody := doJSONAuth(t, http.MethodGet, base+"/blob/"+encBlob.CID, "", nil)
	require.Equal(t, http.StatusOK, postRC, "post-rotation read of encBlob must succeed")
	require.Equal(t, "rotate me", string(postBody))

	// The original signed URL (minted while the signing key was under v1) must still
	// work after rotation (signing key survived re-wrap; exit #2: signing continuity).
	postSigned, _ := getWithOrigin(t, base+signedURL, "https://embed.test")
	require.Equal(t, http.StatusOK, postSigned, "signed URL minted under v1 must still work after rotation to v2")

	// --- Authz on rotate-master ---

	// Moderator must get 403.
	modCode, _ := doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/keys/rotate-master", modTok,
		map[string]any{"from_version": "v1", "to_version": "v2"})
	require.Equal(t, http.StatusForbidden, modCode, "moderator must not be able to trigger rotation")

	// No token must get 401.
	anonCode, _ := doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/keys/rotate-master", "",
		map[string]any{"from_version": "v1", "to_version": "v2"})
	require.Equal(t, http.StatusUnauthorized, anonCode, "unauthenticated request must be rejected")
}

// m10AssertCountZero runs a COUNT(*) query with a single pgtype.UUID argument
// and fails the test if the count is nonzero.
func m10AssertCountZero(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, arg pgtype.UUID, msg string) {
	t.Helper()
	var n int64
	require.NoError(t, pool.QueryRow(ctx, q, arg).Scan(&n))
	require.Zero(t, n, msg)
}

// startCoordinatorWithNginxCfg is a variant of startCoordinatorWithNginx that
// accepts a fully-specified coordinator.Config instead of building one
// internally. This lets the caller inject MasterKeyRotation (or any other
// Config field) without modifying the shared helper.
func startCoordinatorWithNginxCfg(t *testing.T, ctx context.Context, pool *pgxpool.Pool, backend ipfs.Backend, ks *envelope.Keystore, cfg coordinator.Config, nginx func(*testing.T, context.Context, string) string) string {
	t.Helper()
	port := m10ExtractPort(t, cfg.ListenAddr)
	c, err := coordinator.New(pool, backend, ks, cfg)
	require.NoError(t, err)
	runCtx, runCancel := context.WithCancel(ctx)
	t.Cleanup(runCancel)
	go func() { _ = c.Run(runCtx) }()
	require.Eventually(t, func() bool { return c.Addr() != "" }, 5*time.Second, 20*time.Millisecond)
	return nginx(t, ctx, port)
}

// m10ExtractPort returns the port part of an "addr:port" string.
func m10ExtractPort(t *testing.T, addr string) string {
	t.Helper()
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	t.Fatalf("m10ExtractPort: no colon in addr %q", addr)
	return ""
}

// startNginxM10 fronts the coordinator for M10 — same proxy surface as M9
// (the M10 admin routes live under /api/v1/admin/ which is already proxied).
func startNginxM10(t *testing.T, ctx context.Context, coordPort string) string {
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
}
`, up, up, up, up, up)

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
