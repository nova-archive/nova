package storage_test

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

// newKuboBackend builds an offline embedded Kubo backend for storage tests.
// (resolve_test.go is in this same package but does not define this helper.)
func newKuboBackend(t *testing.T, ctx context.Context) ipfs.Backend {
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

func TestOpenBytesPlaintext(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres + kubo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)

	be := newKuboBackend(t, ctx)
	svc := storage.NewService(pool, be, nil) // keystore unused for plaintext

	plaintext := []byte("public archival plaintext, no envelope")
	res, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be},
		blobfixture.Spec{Plaintext: plaintext, MIME: "text/plain", Unencrypted: true})
	require.NoError(t, err)

	view, err := svc.Resolve(ctx, res.CID)
	require.NoError(t, err)
	require.False(t, view.Encrypted, "public_archival blob must resolve as unencrypted")
	require.Equal(t, storage.VisibilityPublic, view.Visibility)

	rc, err := svc.OpenBytes(ctx, view)
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

func TestOpenBytesEncrypted(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres + kubo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)

	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	be := newKuboBackend(t, ctx)
	svc := storage.NewService(pool, be, ks)

	plaintext := []byte("decrypt me through the storage service")
	res, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: plaintext, MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)

	view, err := svc.Resolve(ctx, res.CID)
	require.NoError(t, err)
	require.True(t, view.Encrypted)

	rc, err := svc.OpenBytes(ctx, view)
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}
