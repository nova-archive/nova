package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/stretchr/testify/require"
)

func decodeReady(t *testing.T, body []byte) (bool, []map[string]any) {
	t.Helper()
	var out struct {
		OK     bool             `json:"ok"`
		Checks []map[string]any `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.OK, out.Checks
}

func TestReadyHandler_AllChecksPass(t *testing.T) {
	t.Parallel()
	h := handlers.NewReadyHandler(time.Second,
		handlers.ReadyCheck{Name: "a", Fn: func(ctx context.Context) error { return nil }},
		handlers.ReadyCheck{Name: "b", Fn: func(ctx context.Context) error { return nil }},
	)
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	ok, checks := decodeReady(t, rec.Body.Bytes())
	require.True(t, ok)
	require.Len(t, checks, 2)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestReadyHandler_OneCheckFails(t *testing.T) {
	t.Parallel()
	h := handlers.NewReadyHandler(time.Second,
		handlers.ReadyCheck{Name: "database", Fn: func(ctx context.Context) error { return nil }},
		handlers.ReadyCheck{Name: "ipfs", Fn: func(ctx context.Context) error { return errors.New("kubo down") }},
	)
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	ok, checks := decodeReady(t, rec.Body.Bytes())
	require.False(t, ok)
	require.Len(t, checks, 2)
	var failed map[string]any
	for _, c := range checks {
		if c["name"] == "ipfs" {
			failed = c
		}
	}
	require.NotNil(t, failed)
	require.Equal(t, false, failed["ok"])
	require.Equal(t, "kubo down", failed["err"])
}

func TestReadyHandler_EmptyChecksAlwaysOK(t *testing.T) {
	t.Parallel()
	h := handlers.NewReadyHandler(time.Second)
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	ok, checks := decodeReady(t, rec.Body.Bytes())
	require.True(t, ok)
	require.Empty(t, checks)
}

func TestReadyHandler_SlowCheckTimesOut(t *testing.T) {
	t.Parallel()
	h := handlers.NewReadyHandler(50*time.Millisecond,
		handlers.ReadyCheck{Name: "slow", Fn: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				return nil
			}
		}},
	)
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	ok, checks := decodeReady(t, rec.Body.Bytes())
	require.False(t, ok)
	require.Len(t, checks, 1)
	require.Equal(t, false, checks[0]["ok"])
}

// TestReadyHandler_ChecksRunInParallel ensures the latency is bounded by
// the slowest single check, not the sum — a regression here would silently
// inflate /readyz latency once we wire in more checks.
func TestReadyHandler_ChecksRunInParallel(t *testing.T) {
	t.Parallel()
	const each = 80 * time.Millisecond
	h := handlers.NewReadyHandler(time.Second,
		handlers.ReadyCheck{Name: "a", Fn: func(ctx context.Context) error { time.Sleep(each); return nil }},
		handlers.ReadyCheck{Name: "b", Fn: func(ctx context.Context) error { time.Sleep(each); return nil }},
		handlers.ReadyCheck{Name: "c", Fn: func(ctx context.Context) error { time.Sleep(each); return nil }},
	)
	rec := httptest.NewRecorder()
	start := time.Now()
	h.Serve(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	elapsed := time.Since(start)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Less(t, elapsed, 2*each,
		"three %v checks should run concurrently, completing in well under their sum", each)
}
