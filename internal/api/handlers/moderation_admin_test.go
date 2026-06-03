package handlers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/moderation"
)

// --- fixtures (mirror the moderation package's test setup) -------------------

func modKeystore(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *envelope.Keystore {
	t.Helper()
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)
	return ks
}

func modBackend(t *testing.T, ctx context.Context) ipfs.Backend {
	t.Helper()
	swarm := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarm,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))
	be, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath: t.TempDir(), Mode: ipfs.ModePrivate, SwarmKeyPath: swarm, Online: false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = be.Close(c)
	})
	return be
}

func modService(pool *pgxpool.Pool, be moderation.Backend) *moderation.Service {
	return moderation.NewService(
		gen.New(pool), pool, be, nil,
		auditlog.NewWriter(gen.New(pool), slog.Default()), slog.Default(), time.Now)
}

func modOperator(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		"op-"+uuid.NewString()+"@fixture.test").Scan(&id))
	return id
}

// modReq builds a request with op set as the authenticated operator identity,
// the way the bearer middleware would.
func modReq(method, target string, body string, op uuid.UUID) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	return r.WithContext(auth.ContextWithIdentity(r.Context(),
		auth.Identity{UserID: op.String(), Role: "operator"}))
}

type pageResp struct {
	Data       []map[string]any `json:"data"`
	Pagination struct {
		Page    int `json:"page"`
		PerPage int `json:"per_page"`
		Total   int `json:"total"`
	} `json:"pagination"`
}

func decodePage(t *testing.T, body io.Reader) pageResp {
	t.Helper()
	var out pageResp
	require.NoError(t, json.NewDecoder(body).Decode(&out))
	return out
}

// --- actions: quarantine → takedown ------------------------------------------

func TestModerationQuarantineThenTakedown(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	ks := modKeystore(t, ctx, pool)
	be := modBackend(t, ctx)
	op := modOperator(t, ctx, pool)
	h := handlers.NewModerationAdminHandler(modService(pool, be), gen.New(pool))

	blob, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("quarantine me"), MIME: "image/png", Visibility: "private"})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	h.Quarantine(rec, modReq("POST", "/api/v1/admin/moderation/quarantine",
		`{"cid":"`+blob.CID+`","rule":"dmca","reason":"notice","tombstone_after":"14d"}`, op))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"status":"quarantined"}`, rec.Body.String())
	require.Equal(t, "quarantined", modBlobState(t, ctx, pool, blob.CID))

	// The quarantine recorded the operator as decided_by.
	var decidedBy uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT decided_by FROM moderation_decisions WHERE cid=$1 AND action='quarantine'`,
		blob.CID).Scan(&decidedBy))
	require.Equal(t, op, decidedBy, "actor from context recorded as decided_by")

	rec = httptest.NewRecorder()
	h.Takedown(rec, modReq("POST", "/api/v1/admin/moderation/takedown",
		`{"cid":"`+blob.CID+`","reason":"takedown"}`, op))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"status":"tombstoned"}`, rec.Body.String())
	require.Equal(t, "tombstoned", modBlobState(t, ctx, pool, blob.CID))
}

// Quarantine under legal hold then a takedown is refused with 409 legal_hold;
// after clear-legal-hold the takedown succeeds.
func TestModerationLegalHoldBlocksTakedown(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	ks := modKeystore(t, ctx, pool)
	be := modBackend(t, ctx)
	op := modOperator(t, ctx, pool)
	h := handlers.NewModerationAdminHandler(modService(pool, be), gen.New(pool))

	blob, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("severe"), MIME: "image/png", Visibility: "private"})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	h.Quarantine(rec, modReq("POST", "/q",
		`{"cid":"`+blob.CID+`","rule":"severe_content","reason":"severe","legal_hold":true}`, op))
	require.Equal(t, 200, rec.Code, rec.Body.String())

	rec = httptest.NewRecorder()
	h.Takedown(rec, modReq("POST", "/t", `{"cid":"`+blob.CID+`","reason":"x"}`, op))
	require.Equal(t, 409, rec.Code, rec.Body.String())
	var errBody struct{ Code string }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Equal(t, "legal_hold", errBody.Code)
	require.Equal(t, "quarantined", modBlobState(t, ctx, pool, blob.CID), "refused tombstone rolled back")

	rec = httptest.NewRecorder()
	h.ClearLegalHold(rec, modReq("POST", "/c", `{"cid":"`+blob.CID+`","reason":"released"}`, op))
	require.Equal(t, 200, rec.Code, rec.Body.String())

	rec = httptest.NewRecorder()
	h.Takedown(rec, modReq("POST", "/t", `{"cid":"`+blob.CID+`","reason":"after clear"}`, op))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, "tombstoned", modBlobState(t, ctx, pool, blob.CID))
}

// Restore on a never-quarantined (active) CID is a 409 conflict.
func TestModerationRestoreNonQuarantinedConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	ks := modKeystore(t, ctx, pool)
	be := modBackend(t, ctx)
	op := modOperator(t, ctx, pool)
	h := handlers.NewModerationAdminHandler(modService(pool, be), gen.New(pool))

	blob, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("active"), MIME: "image/png", Visibility: "private"})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	h.Restore(rec, modReq("POST", "/r", `{"cid":"`+blob.CID+`","reason":"noop"}`, op))
	require.Equal(t, 409, rec.Code, rec.Body.String())
	var errBody struct{ Code string }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Equal(t, "conflict", errBody.Code)
}

// Missing cid on an action ⇒ 400.
func TestModerationActionMissingCID(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	op := modOperator(t, ctx, pool)
	h := handlers.NewModerationAdminHandler(modService(pool, nil), gen.New(pool))

	rec := httptest.NewRecorder()
	h.Quarantine(rec, modReq("POST", "/q", `{"reason":"x"}`, op))
	require.Equal(t, 400, rec.Code, rec.Body.String())

	// A bad tombstone_after ⇒ 400 even with a cid.
	rec = httptest.NewRecorder()
	h.Quarantine(rec, modReq("POST", "/q", `{"cid":"bafy1","tombstone_after":"-3d"}`, op))
	require.Equal(t, 400, rec.Code, rec.Body.String())
}

// --- queue / dmca / blocklist listings ---------------------------------------

func TestModerationQueueAndDMCAListing(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	ks := modKeystore(t, ctx, pool)
	be := modBackend(t, ctx)
	op := modOperator(t, ctx, pool)
	h := handlers.NewModerationAdminHandler(modService(pool, be), gen.New(pool))

	blob, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("queue me"), MIME: "image/png", Visibility: "private"})
	require.NoError(t, err)

	// One DMCA case (so the queue's rule_ref resolves a real case id).
	caseID, err := gen.New(pool).InsertDMCACase(ctx, gen.InsertDMCACaseParams{
		ClaimantName: "Acme", ClaimantEmail: "a@b.c", SwornStatement: "s", TargetCid: blob.CID,
	})
	require.NoError(t, err)
	caseStr := uuid.UUID(caseID.Bytes).String()

	// Quarantine referencing the case → a decision row with decided_by set.
	rec := httptest.NewRecorder()
	h.Quarantine(rec, modReq("POST", "/q",
		`{"cid":"`+blob.CID+`","rule":"dmca","case_id":"`+caseStr+`","reason":"notice"}`, op))
	require.Equal(t, 200, rec.Code, rec.Body.String())

	// Queue lists the decision with non-null decided_by and rule_ref.
	rec = httptest.NewRecorder()
	h.Queue(rec, modReq("GET", "/api/v1/admin/moderation/queue", "", op))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	q := decodePage(t, rec.Body)
	require.Equal(t, 1, q.Pagination.Total)
	require.Len(t, q.Data, 1)
	require.Equal(t, blob.CID, q.Data[0]["cid"])
	require.Equal(t, "quarantine", q.Data[0]["action"])
	require.Equal(t, op.String(), q.Data[0]["decided_by"])
	require.Equal(t, caseStr, q.Data[0]["rule_ref"])
	require.NotNil(t, q.Data[0]["scheduled_tombstone_at"], "dmca quarantine schedules a tombstone")

	// DMCA list + get.
	rec = httptest.NewRecorder()
	h.DMCAList(rec, modReq("GET", "/api/v1/admin/moderation/dmca", "", op))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	dl := decodePage(t, rec.Body)
	require.Equal(t, 1, dl.Pagination.Total)
	require.Equal(t, blob.CID, dl.Data[0]["target_cid"])
	// The quarantine actioned the referenced case.
	require.Equal(t, "actioned", dl.Data[0]["status"])

	rec = httptest.NewRecorder()
	req := modReq("GET", "/api/v1/admin/moderation/dmca/"+caseStr, "", op)
	req = withURLParam(req, "id", caseStr)
	h.DMCAGet(rec, req)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var detail map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	require.Equal(t, "s", detail["sworn_statement"])
	require.Equal(t, blob.CID, detail["target_cid"])

	// A bad uuid ⇒ 400; an unknown uuid ⇒ 404.
	rec = httptest.NewRecorder()
	h.DMCAGet(rec, withURLParam(modReq("GET", "/x", "", op), "id", "not-a-uuid"))
	require.Equal(t, 400, rec.Code)
	rec = httptest.NewRecorder()
	h.DMCAGet(rec, withURLParam(modReq("GET", "/x", "", op), "id", uuid.NewString()))
	require.Equal(t, 404, rec.Code)
}

// Blocklist add → list → remove, plus that add records the operator as added_by.
func TestModerationBlocklistAddListRemove(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	op := modOperator(t, ctx, pool)
	h := handlers.NewModerationAdminHandler(modService(pool, nil), gen.New(pool))

	rec := httptest.NewRecorder()
	h.BlocklistAdd(rec, modReq("POST", "/api/v1/admin/moderation/blocklist",
		`{"cid":"bafyblock1","reason":"abuse"}`, op))
	require.Equal(t, 201, rec.Code, rec.Body.String())

	rec = httptest.NewRecorder()
	h.BlocklistList(rec, modReq("GET", "/api/v1/admin/moderation/blocklist", "", op))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	bl := decodePage(t, rec.Body)
	require.Equal(t, 1, bl.Pagination.Total)
	require.Equal(t, "bafyblock1", bl.Data[0]["cid"])
	require.Equal(t, "abuse", bl.Data[0]["reason"])
	require.Equal(t, "operator_manual", bl.Data[0]["rule"])
	require.Equal(t, op.String(), bl.Data[0]["added_by"])

	rec = httptest.NewRecorder()
	h.BlocklistRemove(rec, withURLParam(modReq("DELETE", "/x", "", op), "cid", "bafyblock1"))
	require.Equal(t, 204, rec.Code)

	rec = httptest.NewRecorder()
	h.BlocklistList(rec, modReq("GET", "/api/v1/admin/moderation/blocklist", "", op))
	require.Equal(t, 200, rec.Code)
	require.Equal(t, 0, decodePage(t, rec.Body).Pagination.Total)
}

// --- helpers -----------------------------------------------------------------

func modBlobState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) string {
	t.Helper()
	var s string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM blobs WHERE cid=$1`, cidStr).Scan(&s))
	return s
}

// withURLParam attaches a chi URL param so a handler reading chi.URLParam works
// without a full router mount.
func withURLParam(r *http.Request, key, val string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}
