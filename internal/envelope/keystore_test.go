package envelope_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	got, err := ks.Unwrap(wrapped, versionID)
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
	got, err := ks2.Unwrap(wrappedV1, v1id)
	require.NoError(t, err)
	require.Equal(t, pbk, got)
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
