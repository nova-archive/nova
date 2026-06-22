package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func wireTimers() wire.ConfigUpdates {
	return wire.ConfigUpdates{HeartbeatIntervalSeconds: 300, PinsPollIntervalSeconds: 600, MaxPinConcurrency: 16}
}

func newTestServerPool(t *testing.T) (*Server, *pgxpool.Pool, []byte, []byte) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	s := New(gen.New(pool), Config{Timers: wireTimers()})
	return s, pool, caPEM, caKeyPEM
}

func assignViaSeam(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string, node uuid.UUID) {
	t.Helper()
	tx, _ := pool.Begin(ctx)
	if _, err := AssignPin(ctx, tx, cid, node); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestChangesEmptyThenRows(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	// empty: no changes, next_seq=0, current_epoch=0
	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	var resp wire.ChangesResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != 200 || len(resp.Changes) != 0 || resp.CurrentEpoch != 0 {
		t.Fatalf("empty changes: code=%d resp=%+v", w.Code, resp)
	}

	seedBlob(t, ctx, pool, "bafy1", 5)
	assignViaSeam(t, ctx, pool, "bafy1", id)

	w = httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Changes) != 1 || resp.Changes[0].Kind != "assign" || resp.Changes[0].CID != "bafy1" || resp.Changes[0].ByteSize != 5 {
		t.Fatalf("changes after assign: %+v", resp)
	}
	if resp.NextSeq != resp.Changes[0].Sequence || resp.CurrentEpoch != resp.Changes[0].Sequence {
		t.Fatalf("next_seq/epoch: %+v", resp)
	}
}

func TestChangesSnapshotRequiredBelowWatermark(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	// advance the watermark directly
	pool.Exec(ctx, `UPDATE federation_change_log_state SET pruned_through_seq=100 WHERE id=true`)

	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=50", nil, leaf))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	var er wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &er)
	if er.Code != wire.CodeSnapshotRequired {
		t.Fatalf("code = %q", er.Code)
	}
}

func TestChangesRevokedNode403(t *testing.T) {
	s, _, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	s.q.RevokeNode(context.Background(), pgUUIDFrom(id))
	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
}
