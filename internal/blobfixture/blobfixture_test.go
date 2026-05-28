package blobfixture_test

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/stretchr/testify/require"
)

func newBackend(t *testing.T, ctx context.Context) ipfs.Backend {
	t.Helper()
	swarm := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarm,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))
	be, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath: t.TempDir(), Mode: ipfs.ModePrivate, SwarmKeyPath: swarm, Online: false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = be.Close(c)
	})
	return be
}

func TestFixtureEncryptedRoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres + kubo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)

	masterHex := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	t.Setenv("NOVA_MASTER_KEY_V1", masterHex)
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	be := newBackend(t, ctx)
	plaintext := []byte("nova fixture plaintext payload")

	res, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: plaintext, MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)

	rc, err := be.Get(ctx, res.ParsedCID)
	require.NoError(t, err)
	env, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	_, codec, err := envelope.Decode(env)
	require.NoError(t, err)
	got, err := codec.Decrypt(env, res.PerBlobKey)
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, plaintext))
}
