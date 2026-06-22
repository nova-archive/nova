package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func TestSnapshotPagingAndEpoch(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	for _, c := range []string{"bafa", "bafb", "bafc"} {
		seedBlob(t, ctx, pool, c, 1)
		assignViaSeam(t, ctx, pool, c, id)
	}

	// page 1 (limit 2) captures epoch, returns cursor
	w := httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet, "/fed/v1/pins/snapshot?limit=2", nil, leaf))
	var p1 wire.SnapshotResponse
	json.Unmarshal(w.Body.Bytes(), &p1)
	if w.Code != 200 || len(p1.Data) != 2 || p1.Cursor == "" || p1.SnapshotEpoch == 0 {
		t.Fatalf("page1: code=%d %+v", w.Code, p1)
	}

	// page 2 with epoch + cursor returns the rest, empty cursor
	w = httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet,
		"/fed/v1/pins/snapshot?limit=2&cursor="+p1.Cursor+"&snapshot_epoch="+itoa(p1.SnapshotEpoch), nil, leaf))
	var p2 wire.SnapshotResponse
	json.Unmarshal(w.Body.Bytes(), &p2)
	if len(p2.Data) != 1 || p2.Cursor != "" {
		t.Fatalf("page2: %+v", p2)
	}
}

func TestSnapshot409OnConcurrentSameNodeChange(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	for _, c := range []string{"bafa", "bafb"} {
		seedBlob(t, ctx, pool, c, 1)
		assignViaSeam(t, ctx, pool, c, id)
	}
	w := httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet, "/fed/v1/pins/snapshot?limit=1", nil, leaf))
	var p1 wire.SnapshotResponse
	json.Unmarshal(w.Body.Bytes(), &p1)

	// a new change for THIS node appears mid-pagination
	seedBlob(t, ctx, pool, "bafc", 1)
	assignViaSeam(t, ctx, pool, "bafc", id)

	w = httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet,
		"/fed/v1/pins/snapshot?limit=1&cursor="+p1.Cursor+"&snapshot_epoch="+itoa(p1.SnapshotEpoch), nil, leaf))
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
