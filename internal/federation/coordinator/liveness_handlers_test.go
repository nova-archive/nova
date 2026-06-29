package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// setNodeRaw forces a node's status / sync-state for endpoint-matrix and
// recovery tests (simulating the liveness sweeper having already transitioned it).
func setNodeRaw(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, status, sync string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`UPDATE nodes SET status=$2, assignment_sync_state=$3 WHERE id=$1::uuid`,
		id.String(), status, sync)
	if err != nil {
		t.Fatal(err)
	}
}

func syncState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT assignment_sync_state FROM nodes WHERE id=$1::uuid`, id.String()).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func statusOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM nodes WHERE id=$1::uuid`, id.String()).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRegisterOnConflictSetsSnapshotRequired(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	// Even a fresh registration is snapshot_required: no desired set is synced yet.
	if got := syncState(t, ctx, pool, id); got != "snapshot_required" {
		t.Fatalf("fresh register sync_state = %q, want snapshot_required", got)
	}

	// A node that resynced to current and then re-registers must go back to
	// snapshot_required (its desired set must be (re)synced).
	setNodeRaw(t, ctx, pool, id, "active", "current")
	body, _ := json.Marshal(wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("re-register = %d (%s)", w.Code, w.Body)
	}
	if got := syncState(t, ctx, pool, id); got != "snapshot_required" {
		t.Fatalf("re-register sync_state = %q, want snapshot_required", got)
	}
	if got := statusOf(t, ctx, pool, id); got != "active" {
		t.Fatalf("re-register status = %q, want active", got)
	}
}

func TestHeartbeatReactivatesSetsReconcilingNotCountable(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	// Pretend the sweeper marked it unreachable while it was offline.
	setNodeRaw(t, ctx, pool, id, "unreachable", "current")

	body, _ := json.Marshal(wire.HeartbeatRequest{FreeBytes: 1, StoredBytes: 2})
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", body, leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("recovery heartbeat = %d (%s)", w.Code, w.Body)
	}
	if got := statusOf(t, ctx, pool, id); got != "active" {
		t.Fatalf("status after recovery heartbeat = %q, want active", got)
	}
	// unreachable→active had pending divergence ⇒ reconciling (not countable until
	// it resyncs to the current epoch).
	if got := syncState(t, ctx, pool, id); got != "reconciling" {
		t.Fatalf("sync_state after recovery = %q, want reconciling", got)
	}
}

func TestEvictedHeartbeatRejected(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	setNodeRaw(t, ctx, pool, id, "evicted", "current")

	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), leaf))
	if w.Code != http.StatusForbidden {
		t.Fatalf("evicted heartbeat = %d, want 403", w.Code)
	}
	// And it must NOT have been silently reactivated.
	if got := statusOf(t, ctx, pool, id); got != "evicted" {
		t.Fatalf("evicted node reactivated by heartbeat: status=%q", got)
	}
}

func TestSyncStateCurrentOnlyAtHeadOrSnapshotDone(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	setNodeRaw(t, ctx, pool, id, "active", "reconciling")

	seedBlob(t, ctx, pool, "b1", 5)
	assignViaSeam(t, ctx, pool, "b1", id)
	seedBlob(t, ctx, pool, "b2", 5)
	assignViaSeam(t, ctx, pool, "b2", id)

	// Full page (len == limit) ⇒ more may remain ⇒ stays reconciling.
	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0&limit=1", nil, leaf))
	var r1 wire.ChangesResponse
	json.Unmarshal(w.Body.Bytes(), &r1)
	if len(r1.Changes) != 1 {
		t.Fatalf("page 1 len = %d, want 1", len(r1.Changes))
	}
	if got := syncState(t, ctx, pool, id); got != "reconciling" {
		t.Fatalf("after partial page sync_state = %q, want reconciling", got)
	}

	// Second full page — still not drained.
	w = httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq="+strconv.FormatInt(r1.NextSeq, 10)+"&limit=1", nil, leaf))
	var r2 wire.ChangesResponse
	json.Unmarshal(w.Body.Bytes(), &r2)
	if got := syncState(t, ctx, pool, id); got != "reconciling" {
		t.Fatalf("after second page sync_state = %q, want reconciling", got)
	}

	// Drain: a short page (0 rows < limit) means caught up ⇒ current.
	w = httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq="+strconv.FormatInt(r2.NextSeq, 10)+"&limit=1", nil, leaf))
	var r3 wire.ChangesResponse
	json.Unmarshal(w.Body.Bytes(), &r3)
	if len(r3.Changes) != 0 {
		t.Fatalf("drain page len = %d, want 0", len(r3.Changes))
	}
	if got := syncState(t, ctx, pool, id); got != "current" {
		t.Fatalf("after drain sync_state = %q, want current", got)
	}

	// Snapshot final page also marks current. A second node needs a distinct
	// nebula_cert_fingerprint (it is UNIQUE NOT NULL; registerOK leaves it empty).
	id2 := uuid.New()
	leaf2 := issuedClient(t, caPEM, caKeyPEM, id2)
	body2, _ := json.Marshal(wire.RegisterRequest{
		SupportedProtocols:    []string{wire.ProtocolV1},
		NebulaCertFingerprint: "nfp-" + id2.String(),
	})
	w = httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body2, leaf2))
	if w.Code != http.StatusCreated {
		t.Fatalf("register id2 = %d (%s)", w.Code, w.Body)
	}
	setNodeRaw(t, ctx, pool, id2, "active", "reconciling")
	w = httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet, "/fed/v1/pins/snapshot", nil, leaf2))
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot = %d (%s)", w.Code, w.Body)
	}
	if got := syncState(t, ctx, pool, id2); got != "current" {
		t.Fatalf("after snapshot final page sync_state = %q, want current", got)
	}
}

func TestEndpointStatusMatrix(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	// changes: active/suspect ok; unreachable→heartbeat_required; evicted/revoked→403.
	changesCases := []struct {
		status   string
		wantCode int
		wantBody string // substring expected in error code, "" = none
	}{
		{"active", http.StatusOK, ""},
		{"suspect", http.StatusOK, ""},
		{"unreachable", http.StatusForbidden, "heartbeat_required"},
		{"evicted", http.StatusForbidden, "registration_required"},
		{"revoked", http.StatusForbidden, "node_revoked"},
	}
	for _, c := range changesCases {
		setNodeRaw(t, ctx, pool, id, c.status, "current")
		w := httptest.NewRecorder()
		s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
		if w.Code != c.wantCode {
			t.Fatalf("changes[%s] = %d, want %d (%s)", c.status, w.Code, c.wantCode, w.Body)
		}
		if c.wantBody != "" && !contains(w.Body.String(), c.wantBody) {
			t.Fatalf("changes[%s] body %q missing %q", c.status, w.Body, c.wantBody)
		}
	}

	// ack: active/suspect/unreachable get PAST the status gate (404 unknown for a
	// bogus assignment); evicted/revoked are rejected at the gate (403).
	bogus := ackBody(wire.Ack{AssignmentID: uuid.New().String(), Generation: 1, CID: "no-such"})
	ackCases := []struct {
		status  string
		want403 bool
	}{
		{"active", false},
		{"suspect", false},
		{"unreachable", false},
		{"evicted", true},
		{"revoked", true},
	}
	for _, c := range ackCases {
		setNodeRaw(t, ctx, pool, id, c.status, "current")
		w := httptest.NewRecorder()
		s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/no-such/ack", bogus, leaf))
		got403 := w.Code == http.StatusForbidden
		if got403 != c.want403 {
			t.Fatalf("ack[%s] code = %d, want403=%v", c.status, w.Code, c.want403)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
