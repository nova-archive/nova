package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/config/reload"
)

func TestConfigGetReturnsEffectiveAndEffects(t *testing.T) {
	cfg, err := config.LoadFromBytes([]byte(
		"operator:\n  hostname: h.test\n  contact_email: a@b.test\n" +
			"tls:\n  mode: dev-self-signed\n" +
			"orchestrator:\n  replication:\n    factor:\n      important: 2\n"))
	require.NoError(t, err)
	store := reload.New(cfg, nil, map[string]struct{}{"uploads.max_upload_size_bytes": {}})
	h := handlers.NewConfigAdminHandler(store, "/tmp/operator.yaml")

	rec := httptest.NewRecorder()
	h.Get(rec, operatorReq(http.MethodGet, "/api/v1/admin/config", "", uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Version uint64                    `json:"version"`
		Config  map[string]any            `json:"config"`
		Fields  map[string]map[string]any `json:"fields"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "live", body.Fields["uploads.cors.enabled"]["effect"])
	require.Equal(t, "restart", body.Fields["auth.issuer_url"]["effect"])
	// orchestrator.* always serializes (no omitempty) and is env-only-inert.
	require.Equal(t, "env-only-inert", body.Fields["orchestrator.tick_interval_seconds"]["effect"])
	require.Equal(t, "env", body.Fields["uploads.max_upload_size_bytes"]["source"])
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}
