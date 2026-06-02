package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/audit/integrity"
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

type integrityListResp struct {
	Data       []map[string]any `json:"data"`
	Pagination struct {
		Page    int `json:"page"`
		PerPage int `json:"per_page"`
		Total   int `json:"total"`
	} `json:"pagination"`
}

// TestIntegrationM8IntegrityAuditsThroughNginx exercises the M8 exit criteria
// end-to-end behind nginx: a real audit run records pass/skip rows for a seeded
// corpus; dropping a Kubo pin makes the next kubo_pin_present audit report a
// failure that surfaces in the paginated admin listing; and the listing honors
// result/audit_kind filters, pagination, and the operator/moderator guard.
func TestIntegrationM8IntegrityAuditsThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M8 integration test in short mode")
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

	const coordPort = "19008"
	base := startCoordinatorWithNginx(t, ctx, pool, backend, ks, authCfg, coordPort, startNginxM6)

	const pw = "hunter2hunter2"
	_ = seedAuthUser(t, ctx, pool, "op@example.com", "operator", pw)
	_ = seedAuthUser(t, ctx, pool, "mod@example.com", "moderator", pw)
	_ = seedAuthUser(t, ctx, pool, "up@example.com", "uploader", pw)

	// Seed a corpus exercising every check: an encrypted blob, a public_archival
	// (unencrypted) blob, a multi-block encrypted blob (>1 MiB ⇒ dag-pb), and a
	// pinned derivative.
	enc, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("encrypted payload"), MIME: "text/plain", Visibility: "private"})
	require.NoError(t, err)
	_, err = blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("public archival bytes"), MIME: "text/plain", Visibility: "public", Unencrypted: true})
	require.NoError(t, err)
	big := bytes.Repeat([]byte("nova-multiblock-"), 100000) // ~1.6 MiB ⇒ multi-block
	_, err = blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: big, MIME: "application/octet-stream", Visibility: "public"})
	require.NoError(t, err)
	deriv, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("derivative bytes"), MIME: "image/webp", Visibility: "public"})
	require.NoError(t, err)
	// Reparent the (pinned, real-CID) derivative blob onto enc so
	// derivative_state_consistent has a parent/child pair to check.
	_, err = pool.Exec(ctx,
		`UPDATE blobs SET parent_cid=$1, derivative_preset='thumb', derivative_format='webp' WHERE cid=$2`,
		enc.CID, deriv.CID)
	require.NoError(t, err)

	// Drive the audit subsystem directly: the coordinator's scheduler is dormant
	// (audits disabled in the test Config), but it owns the HTTP listing and the
	// boot-time partition maintainer.
	q := gen.New(pool)
	sched := integrity.NewScheduler(integrity.NewChecks(q, backend, ks), integrity.DefaultCadences(),
		integrity.NewRecorder(q, integrity.NewNoopSink(), nil), q, nil)
	sched.RunOnce(ctx)

	opTok, _ := m6Login(t, base, "op@example.com", pw)

	// 1. The listing shows pass rows and (so far) no failures.
	all := listAudits(t, base, opTok, "")
	require.Positive(t, all.Pagination.Total, "RunOnce recorded rows")
	require.Positive(t, listAudits(t, base, opTok, "?result=pass").Pagination.Total)
	require.Zero(t, listAudits(t, base, opTok, "?result=fail").Pagination.Total,
		"a healthy corpus has no failures before the pin is dropped")

	// 2. Canonical spec test: drop a Kubo pin, re-run kubo_pin_present, and the
	//    failure surfaces in the listing for that exact CID.
	require.NoError(t, backend.Unpin(ctx, enc.ParsedCID))
	sched.RunKind(ctx, integrity.KindKuboPinPresent)
	pinFails := listAudits(t, base, opTok, "?result=fail&audit_kind=kubo_pin_present")
	require.Positive(t, pinFails.Pagination.Total)
	require.True(t, containsCID(pinFails.Data, enc.CID), "the unpinned blob's failure should surface")

	// 3. Pagination caps the page window without changing the total.
	page := listAudits(t, base, opTok, "?per_page=1")
	require.Len(t, page.Data, 1)
	require.Equal(t, 1, page.Pagination.PerPage)
	require.Positive(t, page.Pagination.Total)

	// 4. Authz: operator + moderator 200; no token 401; uploader 403.
	modTok, _ := m6Login(t, base, "mod@example.com", pw)
	upTok, _ := m6Login(t, base, "up@example.com", pw)
	requireListStatus(t, base, opTok, http.StatusOK)
	requireListStatus(t, base, modTok, http.StatusOK)
	requireListStatus(t, base, "", http.StatusUnauthorized)
	requireListStatus(t, base, upTok, http.StatusForbidden)

	// 5. Create-ahead: the boot-time maintainer provisioned the next monthly
	//    partition so inserts won't hit the 2026-07-01 cliff.
	require.Eventually(t, func() bool {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'integrity_audits_2026_07'`).Scan(&n)
		return n > 0
	}, 5*time.Second, 50*time.Millisecond, "maintainer should create the next month's partition")
}

func listAudits(t *testing.T, base, token, query string) integrityListResp {
	t.Helper()
	code, body := doJSONAuth(t, http.MethodGet, base+"/api/v1/admin/audits/integrity"+query, token, nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var out integrityListResp
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func requireListStatus(t *testing.T, base, token string, want int) {
	t.Helper()
	code, _ := doJSONAuth(t, http.MethodGet, base+"/api/v1/admin/audits/integrity", token, nil)
	require.Equal(t, want, code)
}

func containsCID(data []map[string]any, cid string) bool {
	for _, row := range data {
		if row["cid"] == cid {
			return true
		}
	}
	return false
}
