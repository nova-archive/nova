package envelope_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

func mustHexKey() string {
	b := make([]byte, envelope.KeySize)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func TestIntegrationKeystoreBootstrapInsertsRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	require.Equal(t, "v1", ks.ActiveLabel())

	// First call inserts the master_key_versions row.
	versionID, err := ks.Bootstrap(ctx)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, versionID)

	// Second call is a no-op and returns the existing id.
	again, err := ks.Bootstrap(ctx)
	require.NoError(t, err)
	require.Equal(t, versionID, again, "Bootstrap is idempotent")
}

func TestIntegrationKeystoreWrapUnwrapRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(pbk)

	wrapped, versionID, err := ks.Wrap(pbk)
	require.NoError(t, err)
	require.Equal(t, envelope.WrappedKeySize, len(wrapped))

	got, err := ks.Unwrap(ctx, wrapped, versionID)
	require.NoError(t, err)
	require.Equal(t, pbk, got)
}

func TestIntegrationKeystoreMultiVersionUnwrap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)

	// Configure two master-key versions; v1 active.
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_V2", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	v1id, err := ks.Bootstrap(ctx)
	require.NoError(t, err)

	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(pbk)
	wrappedV1, gotID, err := ks.Wrap(pbk)
	require.NoError(t, err)
	require.Equal(t, v1id, gotID, "Wrap uses the active version")

	// Now flip active to v2 and re-init the keystore (simulating restart).
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v2")
	ks2, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	v2id, err := ks2.Bootstrap(ctx)
	require.NoError(t, err)
	require.NotEqual(t, v1id, v2id)

	// We can still unwrap v1-wrapped keys via ks2 because both master
	// keys are loaded in process memory.
	got, err := ks2.Unwrap(ctx, wrappedV1, v1id)
	require.NoError(t, err)
	require.Equal(t, pbk, got)
}

// TestIntegrationKeystoreUnwrapCancelledCtx verifies the M6.2 B6 contract:
// when the in-memory version cache misses and the DB reload path is
// triggered with an already-cancelled context, Unwrap returns promptly
// with a context error rather than hanging on Postgres. This is the
// shutdown-safety guarantee — late requests during graceful drain no
// longer wedge the coordinator.
func TestIntegrationKeystoreUnwrapCancelledCtx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(pbk)
	wrapped, _, err := ks.Wrap(pbk)
	require.NoError(t, err)

	// Construct a versionID that is NOT in the in-memory cache. Any random
	// uuid suffices — Unwrap will fall into the cache-miss reload path.
	missingID := uuid.New()

	cancelledCtx, cancelEarly := context.WithCancel(ctx)
	cancelEarly()

	_, err = ks.Unwrap(cancelledCtx, wrapped, missingID)
	require.Error(t, err, "Unwrap with cancelled ctx + cache miss must error")
	// pgx may wrap the context error; accept either errors.Is or the
	// canonical substring (covers both wrapped and string-rendered forms).
	require.True(t,
		errors.Is(err, context.Canceled) ||
			strings.Contains(err.Error(), "context canceled"),
		"expected context-canceled error; got %v", err)
}

func TestKeystoreRefusesShortHexFromEnv(t *testing.T) {
	t.Setenv("NOVA_MASTER_KEY_V1", "deadbeef") // 4 bytes, too short
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	_, err := envelope.NewKeystoreFromEnv(nil)
	require.Error(t, err)
}

func TestKeystoreRefusesMissingActive(t *testing.T) {
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "")
	_, err := envelope.NewKeystoreFromEnv(nil)
	require.Error(t, err)
}

func TestKeystoreRefusesActiveWithoutLoadedKey(t *testing.T) {
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v2") // active references a version we did not load
	_, err := envelope.NewKeystoreFromEnv(nil)
	require.Error(t, err)
}

func TestKeystoreAccessors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)

	// Load v1 and v2; active = v2.
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_V2", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v2")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	// HasLabel
	require.True(t, ks.HasLabel("v1"), "v1 should be loaded")
	require.True(t, ks.HasLabel("v2"), "v2 should be loaded")
	require.False(t, ks.HasLabel("v3"), "v3 was never loaded")

	// LoadedLabels
	require.Len(t, ks.LoadedLabels(), 2)

	// VersionID — v1 row exists because Bootstrap / loadVersions loaded it from DB
	// (loadVersions scans all rows; v1 is present from a prior Bootstrap if it exists,
	// otherwise only the active v2 row is inserted; for this test we insert v1 first).
	// Insert v1 so loadVersions can cache it, then verify.
	_, err = pool.Exec(ctx,
		`INSERT INTO master_key_versions (version_label, state) VALUES ('v1', 'retired') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	// Re-Bootstrap to populate idByLabel for both labels.
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	v1id, ok := ks.VersionID("v1")
	require.True(t, ok, "VersionID(v1) must resolve after Bootstrap")
	require.NotEqual(t, uuid.Nil, v1id)

	// ActiveVersionID == VersionID("v2")
	v2id, ok := ks.VersionID("v2")
	require.True(t, ok)
	activeID, ok := ks.ActiveVersionID()
	require.True(t, ok)
	require.Equal(t, v2id, activeID, "ActiveVersionID must match VersionID(v2)")
}
