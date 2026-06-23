package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func failBody(f wire.Fail) []byte { b, _ := json.Marshal(f); return b }

func TestAssignUnpinReassignGenerations(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	node := seedNode(t, ctx, pool)
	seedBlob(t, ctx, pool, "bafy1", 1)

	tx, _ := pool.Begin(ctx)
	a1, err := AssignPin(ctx, tx, "bafy1", node)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit(ctx)
	if a1.Generation != 1 {
		t.Fatalf("first assign gen=%d want 1", a1.Generation)
	}

	tx, _ = pool.Begin(ctx)
	u, err := UnpinPin(ctx, tx, "bafy1", node)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit(ctx)
	if u.Generation != 2 {
		t.Fatalf("unpin gen=%d want 2", u.Generation)
	}

	// re-assign after unpin: the row was deleted, so a NEW assignment_id at gen 1
	tx, _ = pool.Begin(ctx)
	a2, err := AssignPin(ctx, tx, "bafy1", node)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit(ctx)
	if a2.Generation != 1 {
		t.Fatalf("re-assign gen=%d want 1 (fresh row)", a2.Generation)
	}
	if a2.AssignmentID == a1.AssignmentID {
		t.Fatal("re-assign after unpin must mint a new assignment_id")
	}
}

func TestAssignRollbackLeavesNoOrphan(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	node := seedNode(t, ctx, pool)
	// no seedBlob → AssignPin's GetBlobSize fails → error → rollback
	tx, _ := pool.Begin(ctx)
	if _, err := AssignPin(ctx, tx, "missing", node); err == nil {
		t.Fatal("expected error for missing blob")
	}
	tx.Rollback(ctx)
	var pa, pc int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_assignments WHERE cid='missing'`).Scan(&pa)
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_changes WHERE cid='missing'`).Scan(&pc)
	if pa != 0 || pc != 0 {
		t.Fatalf("rollback left orphan rows: pin_assignments=%d pin_changes=%d", pa, pc)
	}
}

func TestFailIdempotentStaleAndUnknown(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	seedBlob(t, ctx, pool, "bafy1", 5)
	assignViaSeam(t, ctx, pool, "bafy1", id)
	cur, _ := s.q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{Cid: "bafy1", NodeID: pgUUIDFrom(id)})
	aid := uuid.UUID(cur.AssignmentID.Bytes).String()

	do := func(body []byte, path string) int {
		w := httptest.NewRecorder()
		s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, path, body, leaf))
		return w.Code
	}
	if c := do(failBody(wire.Fail{AssignmentID: aid, Generation: cur.Generation, CID: "bafy1", Reason: "network_error"}), "/fed/v1/pins/bafy1/fail"); c != http.StatusNoContent {
		t.Fatalf("fail = %d want 204", c)
	}
	if c := do(failBody(wire.Fail{AssignmentID: aid, Generation: cur.Generation, CID: "bafy1", Reason: "network_error"}), "/fed/v1/pins/bafy1/fail"); c != http.StatusNoContent {
		t.Fatalf("idempotent re-fail = %d want 204", c)
	}
	if c := do(failBody(wire.Fail{AssignmentID: aid, Generation: cur.Generation - 1, CID: "bafy1", Reason: "network_error"}), "/fed/v1/pins/bafy1/fail"); c != http.StatusConflict {
		t.Fatalf("stale fail = %d want 409", c)
	}
	if c := do(failBody(wire.Fail{AssignmentID: aid, Generation: 1, CID: "nope", Reason: "other"}), "/fed/v1/pins/nope/fail"); c != http.StatusNotFound {
		t.Fatalf("unknown fail = %d want 404", c)
	}
}

func TestPinsMalformedParams(t *testing.T) {
	s, _, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	changes := func(path string) int {
		w := httptest.NewRecorder()
		s.handleChanges(w, reqWithCert(http.MethodGet, path, nil, leaf))
		return w.Code
	}
	if c := changes("/fed/v1/pins/changes?since_seq=lol"); c != http.StatusBadRequest {
		t.Fatalf("since_seq=lol → %d want 400", c)
	}
	if c := changes("/fed/v1/pins/changes?since_seq=-5"); c != http.StatusBadRequest {
		t.Fatalf("negative since_seq → %d want 400", c)
	}
	w := httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet, "/fed/v1/pins/snapshot?snapshot_epoch=abc", nil, leaf))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("snapshot_epoch=abc → %d want 400", w.Code)
	}
}

func TestFailReasonValidation(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	seedBlob(t, ctx, pool, "bafy1", 5)
	assignViaSeam(t, ctx, pool, "bafy1", id)
	cur, _ := s.q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{Cid: "bafy1", NodeID: pgUUIDFrom(id)})
	aid := uuid.UUID(cur.AssignmentID.Bytes).String()

	w := httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/bafy1/fail", failBody(wire.Fail{AssignmentID: aid, Generation: cur.Generation, CID: "bafy1", Reason: "bogus"}), leaf))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bogus reason → %d want 400", w.Code)
	}
	w = httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/bafy1/fail", failBody(wire.Fail{AssignmentID: aid, Generation: cur.Generation, CID: "bafy1"}), leaf))
	if w.Code != http.StatusNoContent {
		t.Fatalf("empty reason (→other) → %d want 204", w.Code)
	}
}

func TestAckCidMismatch(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	seedBlob(t, ctx, pool, "bafy1", 5)
	assignViaSeam(t, ctx, pool, "bafy1", id)
	cur, _ := s.q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{Cid: "bafy1", NodeID: pgUUIDFrom(id)})
	aid := uuid.UUID(cur.AssignmentID.Bytes).String()
	w := httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/bafy1/ack", ackBody(wire.Ack{AssignmentID: aid, Generation: cur.Generation, CID: "other"}), leaf))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cid mismatch → %d want 400", w.Code)
	}
}

func TestRevokedNodeSnapshotAckFail403(t *testing.T) {
	ctx := context.Background()
	s, _, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	s.q.RevokeNode(ctx, pgUUIDFrom(id))
	check := func(method, path string, body []byte) int {
		w := httptest.NewRecorder()
		s.mux().ServeHTTP(w, reqWithCert(method, path, body, leaf))
		return w.Code
	}
	if c := check(http.MethodGet, "/fed/v1/pins/snapshot", nil); c != http.StatusForbidden {
		t.Fatalf("revoked snapshot → %d want 403", c)
	}
	if c := check(http.MethodPost, "/fed/v1/pins/bafy1/ack", ackBody(wire.Ack{AssignmentID: uuid.New().String(), Generation: 1, CID: "bafy1"})); c != http.StatusForbidden {
		t.Fatalf("revoked ack → %d want 403", c)
	}
	if c := check(http.MethodPost, "/fed/v1/pins/bafy1/fail", failBody(wire.Fail{AssignmentID: uuid.New().String(), Generation: 1, CID: "bafy1", Reason: "other"})); c != http.StatusForbidden {
		t.Fatalf("revoked fail → %d want 403", c)
	}
}

func TestChangesNextSeqDoesNotSkipPastDelivered(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	idA := uuid.New()
	leafA := registerOK(t, s, caPEM, caKeyPEM, idA)
	idB := seedNode(t, ctx, pool)

	// node A gets seq 1,2; node B gets seq 3 → global head advances to 3
	for _, c := range []string{"bafa", "bafb"} {
		seedBlob(t, ctx, pool, c, 1)
		assignViaSeam(t, ctx, pool, c, idA)
	}
	seedBlob(t, ctx, pool, "bafc", 1)
	assignViaSeam(t, ctx, pool, "bafc", idB)

	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leafA))
	var resp wire.ChangesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Changes) != 2 {
		t.Fatalf("node A changes = %d, want 2", len(resp.Changes))
	}
	last := resp.Changes[len(resp.Changes)-1].Sequence
	if resp.NextSeq != last {
		t.Fatalf("next_seq = %d, must equal last delivered seq %d (NOT the global head)", resp.NextSeq, last)
	}
	if resp.CurrentEpoch < resp.NextSeq {
		t.Fatalf("current_epoch %d should be >= next_seq %d", resp.CurrentEpoch, resp.NextSeq)
	}
}
