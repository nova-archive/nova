package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

// fakeFailAssigner always returns an error from Assign.
type fakeFailAssigner struct{ called bool }

func (f *fakeFailAssigner) Assign(ctx context.Context, cid, class string) (int, error) {
	f.called = true
	return 0, errors.New("admission: simulated assign failure")
}

// TestGateOffBestEffort verifies that a failing Assigner does NOT cause Put
// to fail — the assign error is swallowed and the upload result is returned.
func TestGateOffBestEffort(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	fb := newFakeBackend()
	fa := &fakeFailAssigner{}

	svc := NewService(pool, fb, ks, WithAssigner(fa))

	body := []byte("gate-off best-effort upload data")
	res, err := svc.Put(ctx, bytes.NewReader(body), int64(len(body)),
		PutContext{MIME: "text/plain", Product: "raw"})

	// Put must succeed even though the assigner returned an error.
	require.NoError(t, err, "Put must succeed even when assigner fails")
	require.NotNil(t, res)
	require.NotEmpty(t, res.CID)

	// The assigner must have been called.
	require.True(t, fa.called, "assigner.Assign must be called after successful commit")
}
