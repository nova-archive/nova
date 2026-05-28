package api_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api"
	"github.com/stretchr/testify/require"
)

func TestWriteError(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	api.WriteError(rec, 404, "not_found", "blob not found", "req-123")

	require.Equal(t, 404, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "not_found", body["code"])
	require.Equal(t, "blob not found", body["message"])
	require.Equal(t, "req-123", body["request_id"])
}
