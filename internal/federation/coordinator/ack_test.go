package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func ackBody(a wire.Ack) []byte { b, _ := json.Marshal(a); return b }

func TestAckSuccessStaleIdempotentUnknown(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	seedBlob(t, ctx, pool, "bafy1", 5)
	assignViaSeam(t, ctx, pool, "bafy1", id)

	cur, _ := s.q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{Cid: "bafy1", NodeID: pgUUIDFrom(id)})
	aid := uuid.UUID(cur.AssignmentID.Bytes).String()

	// success ⇒ 204
	w := httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/bafy1/ack",
		ackBody(wire.Ack{AssignmentID: aid, Generation: cur.Generation, CID: "bafy1"}), leaf))
	if w.Code != http.StatusNoContent {
		t.Fatalf("ack = %d, want 204 (%s)", w.Code, w.Body)
	}

	// idempotent re-ack same generation ⇒ 204
	w = httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/bafy1/ack",
		ackBody(wire.Ack{AssignmentID: aid, Generation: cur.Generation, CID: "bafy1"}), leaf))
	if w.Code != http.StatusNoContent {
		t.Fatalf("idempotent re-ack = %d, want 204", w.Code)
	}

	// stale (older generation) ⇒ 409
	w = httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/bafy1/ack",
		ackBody(wire.Ack{AssignmentID: aid, Generation: cur.Generation - 1, CID: "bafy1"}), leaf))
	if w.Code != http.StatusConflict {
		t.Fatalf("stale ack = %d, want 409", w.Code)
	}

	// unknown cid ⇒ 404
	w = httptest.NewRecorder()
	s.mux().ServeHTTP(w, reqWithCert(http.MethodPost, "/fed/v1/pins/nope/ack",
		ackBody(wire.Ack{AssignmentID: aid, Generation: 1, CID: "nope"}), leaf))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown ack = %d, want 404", w.Code)
	}
}
