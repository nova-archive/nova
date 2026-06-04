package handlers_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/masterkey"
	"github.com/stretchr/testify/require"
)

// mkHexKey returns a random 32-byte key as a hex string.
func mkHexKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, envelope.KeySize)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// newMasterKeyHandler spins up an ephemeral Postgres DB with a two-version
// keystore (v1 + v2, active = v2) and returns a ready-to-use handler. It
// mirrors the newTestRotator pattern from internal/masterkey/rotator_test.go.
func newMasterKeyHandler(t *testing.T, ctx context.Context) (*handlers.MasterKeyAdminHandler, *gen.Queries) {
	t.Helper()

	pool := dbtest.New(t, ctx)

	// Load v1 and v2; active = v2.
	t.Setenv("NOVA_MASTER_KEY_V1", mkHexKey(t))
	t.Setenv("NOVA_MASTER_KEY_V2", mkHexKey(t))
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v2")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)

	// Bootstrap inserts the active (v2) row.
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	// Insert a v1 row in 'active' state so BeginVersionRotation can flip it
	// to 'rotating'.
	_, err = pool.Exec(ctx,
		`INSERT INTO master_key_versions (version_label, state) VALUES ('v1', 'active') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	// Re-Bootstrap so loadVersions caches the v1 UUID in the keystore.
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	q := gen.New(pool)
	rot := masterkey.NewRotator(masterkey.Config{
		Q:        q,
		Pool:     pool,
		Keystore: ks,
		Logger:   slog.Default(),
	})

	h := handlers.NewMasterKeyAdminHandler(rot, nil)
	return h, q
}

func TestMasterKeyRotateMaster(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Sub-test: to_not_active — use a fresh DB so state is clean.
	t.Run("to_not_active", func(t *testing.T) {
		h, _ := newMasterKeyHandler(t, ctx)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/",
			strings.NewReader(`{"from_version":"v1","to_version":"v1"}`))
		h.RotateMaster(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		var errBody struct {
			Code string `json:"code"`
		}
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&errBody))
		require.Equal(t, "to_not_active", errBody.Code)
	})

	// Sub-test: happy path + already_rotating — share a DB so the second
	// Start sees the first one's 'rotating' state.
	t.Run("happy_path_then_already_rotating", func(t *testing.T) {
		h, q := newMasterKeyHandler(t, ctx)

		// Happy path: 202 accepted.
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/",
			strings.NewReader(`{"from_version":"v1","to_version":"v2"}`))
		h.RotateMaster(rec, req)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		// Verify DB: v1 must now be rotating.
		row, err := q.GetRotatingVersion(ctx)
		require.NoError(t, err)
		require.Equal(t, "v1", row.VersionLabel)

		// Verify 202 body shape.
		var body map[string]any
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		require.Equal(t, "v1", body["from"])
		require.Equal(t, "v2", body["to"])
		_, hasDEKs := body["total_deks"]
		_, hasSigningKeys := body["total_signing_keys"]
		require.True(t, hasDEKs, "202 body must include total_deks")
		require.True(t, hasSigningKeys, "202 body must include total_signing_keys")
		_, hasStatus := body["status"]
		require.True(t, hasStatus, "202 body must include status")

		// Second call: already rotating → 409.
		rec2 := httptest.NewRecorder()
		req2 := httptest.NewRequest(http.MethodPost, "/",
			strings.NewReader(`{"from_version":"v1","to_version":"v2"}`))
		h.RotateMaster(rec2, req2)
		require.Equal(t, http.StatusConflict, rec2.Code, rec2.Body.String())
		var errBody struct {
			Code string `json:"code"`
		}
		require.NoError(t, json.NewDecoder(rec2.Body).Decode(&errBody))
		require.Equal(t, "rotation_in_progress", errBody.Code)
	})
}

func TestMasterKeyRotationStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	h, _ := newMasterKeyHandler(t, ctx)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.RotationStatus(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var st masterkey.Status
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&st))
	require.Equal(t, "v2", st.Active)
	require.Nil(t, st.InProgress, "no rotation started yet")
	require.NotEmpty(t, st.Versions)
}
