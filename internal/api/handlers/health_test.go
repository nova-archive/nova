package handlers_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/stretchr/testify/require"
)

func TestHealth(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handlers.Health("v0.1.0-test").ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	require.Equal(t, 200, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "ok", body["status"])
	require.Equal(t, "v0.1.0-test", body["version"])
	require.NotEmpty(t, body["time"])
}
