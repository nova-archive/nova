package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/stretchr/testify/require"
)

func TestHeartbeatPersistsEgressTelemetry(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	body, _ := json.Marshal(wire.HeartbeatRequest{
		FreeBytes:                  10,
		StoredBytes:                20,
		EgressBudgetRemainingBytes: 4096,
		EgressBudgetCapacityBytes:  10240,
		EgressRefillBytesPerSecond: 7,
	})
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", body, leaf))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var remaining, capacity, refill int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT last_egress_remaining_bytes, last_egress_capacity_bytes, last_egress_refill_bps
		FROM nodes WHERE id=$1::uuid`, id.String()).Scan(&remaining, &capacity, &refill))
	require.Equal(t, int64(4096), remaining)
	require.Equal(t, int64(10240), capacity)
	require.Equal(t, int64(7), refill)
}

func TestHeartbeatWithoutTelemetryLeavesColumnsNull(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	// A non-reporting donor (capacity 0) must not clobber the columns to 0.
	body, _ := json.Marshal(wire.HeartbeatRequest{FreeBytes: 1, StoredBytes: 2})
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", body, leaf))
	require.Equal(t, http.StatusOK, w.Code)

	var allNull bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT last_egress_remaining_bytes IS NULL AND last_egress_capacity_bytes IS NULL
		   AND last_egress_refill_bps IS NULL
		FROM nodes WHERE id=$1::uuid`, id.String()).Scan(&allNull))
	require.True(t, allNull, "no telemetry ⇒ columns stay NULL, not 0")
}
