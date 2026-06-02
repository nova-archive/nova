package handlers_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

func randHexKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// newSigningAdminFixture builds a SigningAdminHandler backed by a fresh
// migrated Postgres, a bootstrapped keystore, and an initial active signing key.
func newSigningAdminFixture(t *testing.T, ctx context.Context) (*handlers.SigningAdminHandler, *gen.Queries) {
	t.Helper()
	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", randHexKey(t))
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	q := gen.New(pool)
	require.NoError(t, signedurl.EnsureActiveKey(ctx, q, ks))
	keys := signedurl.NewKeySource(q, ks, time.Minute)
	revs := signedurl.NewRevocations(q)
	h := handlers.NewSigningAdminHandler(pool, ks, keys, revs, 24*time.Hour, 24*time.Hour)
	return h, q
}

func TestIntegrationRotateSigning(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	h, q := newSigningAdminFixture(t, ctx)

	before, err := q.GetActiveSigningKey(ctx)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/keys/rotate-signing", strings.NewReader(`{"grace_seconds": 2}`))
	h.RotateSigning(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp struct {
		KID            string `json:"kid"`
		GraceExpiresAt string `json:"grace_expires_at"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEqual(t, before.Kid, resp.KID, "rotation issues a fresh kid")
	require.NotEmpty(t, resp.GraceExpiresAt)

	after, err := q.GetActiveSigningKey(ctx)
	require.NoError(t, err)
	require.Equal(t, resp.KID, after.Kid, "new active key is the rotated kid")

	count, err := q.CountActiveSigningKeys(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), count, "exactly one active key after rotation")

	prior, err := q.GetSigningKeyByKID(ctx, before.Kid)
	require.NoError(t, err)
	require.Equal(t, gen.KeyStateRetired, prior.State, "prior active key retired")
	require.True(t, prior.RetireAfter.Valid && prior.RetireAfter.Time.After(time.Now()), "retire_after within grace")
}

func revoke(h *handlers.SigningAdminHandler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.RevokeSignedURL(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/signed-urls/revoke", strings.NewReader(body)))
	return rec
}

func TestIntegrationRevokeSignedURL(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	h, q := newSigningAdminFixture(t, ctx)

	rec := revoke(h, `{"kind":"cid","value":"bafyX"}`)
	require.Equal(t, http.StatusCreated, rec.Code)
	rows, err := q.ListRevocations(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "cid", rows[0].Kind)
	require.Equal(t, "bafyX", rows[0].Value)

	// Idempotent on the unique (kind, value) pair.
	require.Equal(t, http.StatusCreated, revoke(h, `{"kind":"cid","value":"bafyX"}`).Code)
	rows, err = q.ListRevocations(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "duplicate revocation is a no-op")

	// Invalid kind → 400 invalid_kind.
	bad := revoke(h, `{"kind":"bogus","value":"x"}`)
	require.Equal(t, http.StatusBadRequest, bad.Code)
	require.Contains(t, bad.Body.String(), "invalid_kind")

	// Missing value → 400.
	require.Equal(t, http.StatusBadRequest, revoke(h, `{"kind":"aud","value":""}`).Code)
}
