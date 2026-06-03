package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/auditlog"
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
	"github.com/nova-archive/nova/internal/moderation"
	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestIntegrationM9ModerationThroughNginx proves the three M9 exit criteria
// end-to-end behind nginx, plus the blocklist deny, the audit-log trail, and the
// admin authz matrix. The coordinator owns the moderation HTTP surface (intake +
// admin); the scheduled-tombstone job is driven deterministically by a test
// sweeper over the same pool+backend (the coordinator's sweep is dormant in the
// test Config, mirroring how the M8 test drives the audit scheduler directly).
func TestIntegrationM9ModerationThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M9 integration test in short mode")
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

	const coordPort = "19009"
	base := startCoordinatorWithNginx(t, ctx, pool, backend, ks, authCfg, coordPort, startNginxM9)

	const pw = "hunter2hunter2"
	_ = seedAuthUser(t, ctx, pool, "op@example.com", "operator", pw)
	_ = seedAuthUser(t, ctx, pool, "mod@example.com", "moderator", pw)
	_ = seedAuthUser(t, ctx, pool, "up@example.com", "uploader", pw)
	opTok, _ := m6Login(t, base, "op@example.com", pw)
	modTok, _ := m6Login(t, base, "mod@example.com", pw)
	upTok, _ := m6Login(t, base, "up@example.com", pw)

	// Parent-only blobs (the derivative-DEK cascade is unit-tested): a DMCA
	// target, a severe-content target, and a public_archival (deterministic-CID)
	// blocklist target.
	enc, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("dmca payload"), MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)
	sev, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("severe payload"), MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)
	pub, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("blocklisted bytes"), MIME: "text/plain", Visibility: "public", Unencrypted: true})
	require.NoError(t, err)

	// A test-driven sweeper over the same pool+backend; we force schedules overdue
	// and Tick deterministically rather than waiting on wall-clock cadence.
	modSvc := moderation.NewService(gen.New(pool), pool, backend, nil,
		auditlog.NewWriter(gen.New(pool), slog.Default()), slog.Default(), time.Now)
	sweeper := moderation.NewSweeper(modSvc, time.Minute, true, slog.Default())

	// ---- Exit #1: DMCA quarantine → scheduled-tombstone job → crypto-shred + unpin.
	caseCode, caseBody := postJSON(t, base+"/legal/dmca", map[string]any{
		"claimant_name": "Claimant", "claimant_email": "c@law.test",
		"sworn_statement": "I swear under penalty of perjury", "target_cid": enc.CID})
	require.Equal(t, http.StatusAccepted, caseCode, string(caseBody))
	var caseResp struct {
		CaseID string `json:"case_id"`
	}
	require.NoError(t, json.Unmarshal(caseBody, &caseResp))
	require.NotEmpty(t, caseResp.CaseID)
	require.Positive(t, m9ListTotal(t, base, opTok, "/api/v1/admin/moderation/dmca"), "intake records a case")

	qCode, qBody := doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/moderation/quarantine", opTok,
		map[string]any{"cid": enc.CID, "case_id": caseResp.CaseID, "reason": "DMCA notice", "tombstone_after": "1h"})
	require.Equal(t, http.StatusOK, qCode, string(qBody))

	rc, _ := doJSONAuth(t, http.MethodGet, base+"/blob/"+enc.CID, "", nil)
	require.Equal(t, http.StatusUnavailableForLegalReasons, rc, "quarantined reads return 451")

	m9SetSchedulePast(t, ctx, pool, enc.CID)
	sweeper.Tick(ctx)

	rc, _ = doJSONAuth(t, http.MethodGet, base+"/blob/"+enc.CID, "", nil)
	require.Equal(t, http.StatusGone, rc, "tombstoned reads return 410")
	require.Equal(t, "shredded", m9DEKState(t, ctx, pool, enc.CID), "DEK is crypto-shredded")
	has, _ := backend.Has(ctx, enc.ParsedCID)
	require.False(t, has, "tombstone unpins the CID")

	// ---- Exit #2: severe-content quarantine --legal-hold ⇒ shred refused at DB.
	qCode, qBody = doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/moderation/quarantine", opTok,
		map[string]any{"cid": sev.CID, "reason": "severe content review", "legal_hold": true})
	require.Equal(t, http.StatusOK, qCode, string(qBody))
	tCode, tBody := doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/moderation/takedown", opTok,
		map[string]any{"cid": sev.CID, "reason": "attempt"})
	require.Equal(t, http.StatusConflict, tCode, string(tBody)) // 409 legal_hold (CHECK-enforced)
	require.Equal(t, "active", m9DEKState(t, ctx, pool, sev.CID), "DEK is NOT shredded under legal hold")

	// ---- Exit #3: clear-legal-hold (operator-only) ⇒ tombstone permitted.
	mc, _ := doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/moderation/clear-legal-hold", modTok,
		map[string]any{"cid": sev.CID, "reason": "x"})
	require.Equal(t, http.StatusForbidden, mc, "clear-legal-hold is operator-only")
	oc, ocBody := doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/moderation/clear-legal-hold", opTok,
		map[string]any{"cid": sev.CID, "reason": "preservation window expired"})
	require.Equal(t, http.StatusOK, oc, string(ocBody))
	sweeper.Tick(ctx) // clear-legal-hold set scheduled_tombstone_at=now(); the sweep tombstones it
	require.Equal(t, "shredded", m9DEKState(t, ctx, pool, sev.CID), "tombstone permitted once legal hold is cleared")

	// ---- Blocklist: deny a public_archival CID end-to-end (read → 451).
	bc, bBody := doJSONAuth(t, http.MethodPost, base+"/api/v1/admin/moderation/blocklist", opTok,
		map[string]any{"cid": pub.CID, "reason": "operator block"})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, bc, string(bBody))
	rc, _ = doJSONAuth(t, http.MethodGet, base+"/blob/"+pub.CID, "", nil)
	require.Equal(t, http.StatusUnavailableForLegalReasons, rc, "blocklisted CID read is denied")
	require.Positive(t, m9ListTotal(t, base, opTok, "/api/v1/admin/moderation/blocklist"))

	// ---- Audit log shows the operator-action trail.
	require.Positive(t, m9ListTotal(t, base, opTok, "/api/v1/admin/audit-log?action=dmca.quarantined"))
	require.Positive(t, m9ListTotal(t, base, opTok, "/api/v1/admin/audit-log?action=dmca.tombstoned"))
	require.Positive(t, m9ListTotal(t, base, opTok, "/api/v1/admin/audit-log?action=severe.legal_hold_cleared"))

	// ---- Authz matrix on the moderation queue.
	require.Equal(t, http.StatusOK, m9GetStatus(t, base, opTok, "/api/v1/admin/moderation/queue"))
	require.Equal(t, http.StatusOK, m9GetStatus(t, base, modTok, "/api/v1/admin/moderation/queue"))
	require.Equal(t, http.StatusUnauthorized, m9GetStatus(t, base, "", "/api/v1/admin/moderation/queue"))
	require.Equal(t, http.StatusForbidden, m9GetStatus(t, base, upTok, "/api/v1/admin/moderation/queue"))

	// ---- The boot-time maintainer create-aheads audit_log partitions (M9).
	require.Eventually(t, func() bool {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname='audit_log_2026_07'`).Scan(&n)
		return n > 0
	}, 5*time.Second, 50*time.Millisecond, "maintainer create-aheads audit_log partitions")
}

func m9SetSchedulePast(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`UPDATE moderation_decisions SET scheduled_tombstone_at = now() - interval '1 hour'
		 WHERE cid=$1 AND action='quarantine'`, cidStr)
	require.NoError(t, err)
}

func m9DEKState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) string {
	t.Helper()
	var s string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT k.state FROM data_encryption_keys k JOIN blobs b ON b.encryption_key_id=k.id WHERE b.cid=$1`,
		cidStr).Scan(&s))
	return s
}

func m9ListTotal(t *testing.T, base, token, path string) int {
	t.Helper()
	code, body := doJSONAuth(t, http.MethodGet, base+path, token, nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var out struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.Pagination.Total
}

func m9GetStatus(t *testing.T, base, token, path string) int {
	t.Helper()
	code, _ := doJSONAuth(t, http.MethodGet, base+path, token, nil)
	return code
}

// startNginxM9 fronts the coordinator like startNginxM6 but also proxies the
// public /legal/ path (the M9 DMCA intake) in addition to /blob, /api/v1/auth,
// /api/v1/users/me, and /api/v1/admin.
func startNginxM9(t *testing.T, ctx context.Context, coordPort string) string {
	t.Helper()
	up := "http://host.testcontainers.internal:" + coordPort
	conf := fmt.Sprintf(`
server {
  listen 8080;
  location = /health          { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /blob/             { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /legal/            { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/auth/      { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location = /api/v1/users/me { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/admin/     { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
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
