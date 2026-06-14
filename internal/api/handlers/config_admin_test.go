package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func writeTempCfg(t *testing.T) string {
	t.Helper()
	cfg, err := config.LoadFromBytes([]byte(
		"operator:\n  hostname: h.test\n  contact_email: a@b.test\n" +
			"tls:\n  mode: dev-self-signed\n" +
			"orchestrator:\n  replication:\n    factor:\n      important: 2\n"))
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "operator.yaml")
	require.NoError(t, config.WriteAtomic(p, cfg))
	return p
}

func TestConfigPatchPersistsAndHotApplies(t *testing.T) {
	p := writeTempCfg(t)
	cfg, err := config.LoadFromFile(p)
	require.NoError(t, err)
	store := reload.New(cfg, nil, nil)
	h := handlers.NewConfigAdminHandler(store, p)

	rec := httptest.NewRecorder()
	h.Update(rec, operatorReq(http.MethodPatch, "/api/v1/admin/config",
		`{"uploads":{"limits":{"max_concurrent_global":8}}}`, uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, 8, store.Load().Uploads.Limits.MaxConcurrentGlobal) // hot-applied
	reloaded, err := config.LoadFromFile(p)
	require.NoError(t, err)
	require.Equal(t, 8, reloaded.Uploads.Limits.MaxConcurrentGlobal) // persisted

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Empty(t, body["restart_required"]) // limits are live
}

func TestConfigPatchReportsRestartRequired(t *testing.T) {
	p := writeTempCfg(t)
	cfg, err := config.LoadFromFile(p)
	require.NoError(t, err)
	store := reload.New(cfg, nil, nil)
	h := handlers.NewConfigAdminHandler(store, p)

	rec := httptest.NewRecorder()
	h.Update(rec, operatorReq(http.MethodPatch, "/api/v1/admin/config",
		`{"auth":{"issuer_url":"https://idp.test/"}}`, uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		RestartRequired []string `json:"restart_required"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Contains(t, body.RestartRequired, "auth.issuer_url")
}

func TestConfigPatchInvalidPersistsNothing(t *testing.T) {
	p := writeTempCfg(t)
	cfg, err := config.LoadFromFile(p)
	require.NoError(t, err)
	store := reload.New(cfg, nil, nil)
	before, err := os.ReadFile(p)
	require.NoError(t, err)
	h := handlers.NewConfigAdminHandler(store, p)

	rec := httptest.NewRecorder()
	h.Update(rec, operatorReq(http.MethodPatch, "/api/v1/admin/config",
		`{"tls":{"mode":"bogus"}}`, uuid.New()))
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	after, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, before, after) // nothing written
}

func TestConfigPatchIfMatchConflict(t *testing.T) {
	p := writeTempCfg(t)
	cfg, err := config.LoadFromFile(p)
	require.NoError(t, err)
	store := reload.New(cfg, nil, nil)
	store.Swap(cfg) // version now 1
	h := handlers.NewConfigAdminHandler(store, p)
	r := operatorReq(http.MethodPatch, "/api/v1/admin/config", `{"tos_url":"https://t.test"}`, uuid.New())
	r.Header.Set("If-Match", "0") // stale
	rec := httptest.NewRecorder()
	h.Update(rec, r)
	require.Equal(t, http.StatusConflict, rec.Code)
}
