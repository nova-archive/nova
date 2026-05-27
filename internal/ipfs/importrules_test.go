package ipfs_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/stretchr/testify/require"
)

func TestRawCodecThresholdIsOneMiB(t *testing.T) {
	t.Parallel()
	require.Equal(t, int64(1<<20), ipfs.RawCodecThresholdBytes,
		"IPFS_IMPORT_RULES.md fixes the threshold at 1 MiB; do not change without spec amendment")
}

func TestChunkerSizeIsTwoFiftySixKiB(t *testing.T) {
	t.Parallel()
	require.Equal(t, int64(262144), ipfs.ChunkerSizeBytes,
		"IPFS_IMPORT_RULES.md fixes the chunker at 256 KiB; do not change without spec amendment")
	require.Equal(t, "size-262144", ipfs.ChunkerSpec,
		"ChunkerSpec must match the Kubo chunker name exactly")
}

func TestShouldUseRawCodecAtAndBelowThreshold(t *testing.T) {
	t.Parallel()
	require.True(t, ipfs.ShouldUseRawCodec(0))
	require.True(t, ipfs.ShouldUseRawCodec(1))
	require.True(t, ipfs.ShouldUseRawCodec(262144))   // exactly one chunk
	require.True(t, ipfs.ShouldUseRawCodec(1<<20))    // at threshold
	require.False(t, ipfs.ShouldUseRawCodec(1<<20+1)) // one byte over
	require.False(t, ipfs.ShouldUseRawCodec(5*1024*1024))
}
