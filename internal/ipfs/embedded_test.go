package ipfs_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/stretchr/testify/require"
)

func newOfflineBackend(t *testing.T) (*ipfs.EmbeddedBackend, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	repo := t.TempDir()
	swarmKey := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarmKey,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))

	be, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath:     repo,
		Mode:         ipfs.ModePrivate,
		SwarmKeyPath: swarmKey,
		Online:       false, // offline = no libp2p swarm in tests
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = be.Close(shutdownCtx)
	})
	return be, ctx
}

func TestIntegrationEmbeddedRoundTripSmall(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	be, ctx := newOfflineBackend(t)

	envelope := bytes.Repeat([]byte{0xAB}, 1024) // 1 KiB, raw-codec path
	res, err := be.AddDeterministic(ctx, envelope)
	require.NoError(t, err)
	require.Equal(t, ipfs.CodecRaw, res.Codec, "≤1MiB envelope must use raw codec")
	require.Equal(t, int64(len(envelope)), res.EnvelopeSize)
	require.Equal(t, 1, len(res.Blocks), "single-block raw must yield exactly one block row")
	require.Equal(t, res.CID, res.Blocks[0].CID, "raw block CID equals envelope CID")
	require.Equal(t, res.CID, res.MerkleRoot)

	rc, err := be.Get(ctx, res.CID)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, envelope, got)

	has, err := be.Has(ctx, res.CID)
	require.NoError(t, err)
	require.True(t, has)
}

func TestIntegrationEmbeddedRoundTripLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	be, ctx := newOfflineBackend(t)

	envelope := make([]byte, 4*1024*1024) // 4 MiB, dag-pb path
	_, _ = rand.Read(envelope)
	res, err := be.AddDeterministic(ctx, envelope)
	require.NoError(t, err)
	require.Equal(t, ipfs.CodecDagPB, res.Codec, ">1MiB envelope must use dag-pb codec")
	require.GreaterOrEqual(t, len(res.Blocks), 2, "multi-block result")
	// Block index ordering MUST be deterministic.
	for i, b := range res.Blocks {
		require.Equal(t, i, b.Index)
		require.Greater(t, b.Size, 0)
	}

	rc, err := be.Get(ctx, res.CID)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, envelope, got)
}

func TestIntegrationEmbeddedSameBytesSameCID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	be1, ctx1 := newOfflineBackend(t)
	be2, ctx2 := newOfflineBackend(t)

	envelope := []byte("deterministic bytes -> identical CID")
	res1, err := be1.AddDeterministic(ctx1, envelope)
	require.NoError(t, err)
	res2, err := be2.AddDeterministic(ctx2, envelope)
	require.NoError(t, err)

	require.Equal(t, res1.CID.String(), res2.CID.String(),
		"identical bytes MUST produce identical CIDs across independently-initialised Kubo nodes")
}

func TestIntegrationEmbeddedUnpin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	be, ctx := newOfflineBackend(t)

	envelope := []byte("to be unpinned")
	res, err := be.AddDeterministic(ctx, envelope)
	require.NoError(t, err)

	require.NoError(t, be.Unpin(ctx, res.CID))

	// After unpin, the local Kubo can garbage-collect; we don't run GC
	// in the test, so the bytes may still be in the blockstore, but Has
	// reports "not pinned" via the absence of the pin record. We assert
	// the weaker invariant: Unpin succeeded without error and re-pin
	// works.
	require.NoError(t, be.Pin(ctx, res.CID))
}

func TestIntegrationEmbeddedBlockGet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	be, ctx := newOfflineBackend(t)

	envelope := make([]byte, 600*1024) // 600 KiB; under threshold so raw
	_, _ = rand.Read(envelope)
	res, err := be.AddDeterministic(ctx, envelope)
	require.NoError(t, err)

	has, err := be.BlockstoreHas(ctx, res.CID)
	require.NoError(t, err)
	require.True(t, has)

	bytesGot, err := be.BlockGet(ctx, res.CID)
	require.NoError(t, err)
	require.Equal(t, envelope, bytesGot,
		"raw block bytes == envelope bytes (no UnixFS wrapping)")
}

func TestIntegrationEmbeddedInstallsSwarmKeyIntoRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	repo := t.TempDir()
	swarmKey := filepath.Join(t.TempDir(), "swarm.key")
	const content = "/key/swarm/psk/1.0.0/\n/base16/\n" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
	require.NoError(t, ipfs.WriteFileForTest(swarmKey, []byte(content)))

	be, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath:     repo,
		Mode:         ipfs.ModePrivate,
		SwarmKeyPath: swarmKey,
		Online:       false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = be.Close(shutdownCtx)
	})

	// The swarm key MUST be installed into the repo so libp2p loads the
	// PSK when the node runs online (M3). Without this, an online private
	// node silently joins the public libp2p network.
	installed, err := os.ReadFile(filepath.Join(repo, "swarm.key"))
	require.NoError(t, err)
	require.Equal(t, content, string(installed),
		"NewEmbedded must copy the operator swarm key into <repo>/swarm.key")
}

func TestIntegrationEmbeddedRefusesOpenOnHardeningViolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	repo := t.TempDir()
	// No swarm key — ValidateConfig must refuse before the node starts.
	_, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath:     repo,
		Mode:         ipfs.ModePrivate,
		SwarmKeyPath: filepath.Join(t.TempDir(), "missing.key"),
		Online:       false,
	})
	require.ErrorIs(t, err, ipfs.ErrSwarmKeyMissing)
}
