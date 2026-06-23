package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func TestChangeLogHeadMonotonicAcrossFullPrune(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	seedBlob(t, ctx, pool, "bafy1", 1)
	assignViaSeam(t, ctx, pool, "bafy1", id)

	headBefore, err := s.q.GetChangeLogHead(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// fully prune the change log (watermark advances to head, table empties)
	if _, err := pool.Exec(ctx, `UPDATE pin_changes SET created_at = now() - interval '30 days'`); err != nil {
		t.Fatal(err)
	}
	if err := s.pruneOnce(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}

	headAfter, err := s.q.GetChangeLogHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if headAfter != headBefore {
		t.Fatalf("head regressed after full prune: before=%d after=%d (must stay >= watermark)", headBefore, headAfter)
	}

	// a donor whose cursor == head must NOT be told snapshot_required (no recovery loop)
	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq="+itoa(headAfter), nil, leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("poll at recovered cursor should be 200 (no loop), got %d (%s)", w.Code, w.Body)
	}
}

func TestPruneAdvancesWatermarkAndTriggersSnapshot(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	seedBlob(t, ctx, pool, "bafy1", 1)
	assignViaSeam(t, ctx, pool, "bafy1", id)
	// backdate the change so it is older than the retention cutoff
	pool.Exec(ctx, `UPDATE pin_changes SET created_at = now() - interval '30 days'`)

	if err := s.pruneOnce(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	var remaining int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_changes`).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("pin_changes remaining = %d, want 0", remaining)
	}
	// a poll below the new watermark now demands snapshot recovery
	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	var er wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &er)
	if w.Code != http.StatusBadRequest || er.Code != wire.CodeSnapshotRequired {
		t.Fatalf("post-prune poll: code=%d %q", w.Code, er.Code)
	}
}
