package ipfs

// IPFS import rules per docs/specs/IPFS_IMPORT_RULES.md. Every value
// here is Tier 1 — protocol-enforced, refused-at-startup if mutated
// across a federation. Operators cannot tune any of these.
const (
	// RawCodecThresholdBytes is the upper bound (inclusive) for the
	// single-block raw-codec import shortcut. Envelopes at or below
	// this size are stored as a single raw block; above this they are
	// chunked under a dag-pb UnixFS file node.
	RawCodecThresholdBytes int64 = 1 << 20 // 1 MiB

	// ChunkerSizeBytes is the fixed chunk size for multi-block imports.
	// Numerically equal to ChunkerSpec's suffix.
	ChunkerSizeBytes int64 = 262144 // 256 KiB

	// ChunkerSpec is the Kubo chunker-name string that produces the
	// ChunkerSizeBytes chunks. Used verbatim as the
	// options.Unixfs.Chunker(...) argument.
	ChunkerSpec = "size-262144"

	// HashAlg is the multihash algorithm name used for every block in
	// every Nova-imported DAG. Matches IPFS_IMPORT_RULES.md.
	HashAlg = "sha2-256"

	// CodecRaw is the multicodec name for the single-block path.
	CodecRaw = "raw"

	// CodecDagPB is the multicodec name for the multi-block path.
	CodecDagPB = "dag-pb"

	// MaxLinkCount is Kubo's UnixFS-1 default. Recorded here to make
	// drift obvious if a future Kubo upgrade changes the default.
	MaxLinkCount = 174
)

// ShouldUseRawCodec returns true when an envelope of envelopeSize bytes
// should be imported via the raw-codec shortcut (single block, no
// UnixFS wrapping). Returns false for sizes above RawCodecThresholdBytes,
// indicating the dag-pb chunked path is required for spec-correct CIDs.
func ShouldUseRawCodec(envelopeSize int64) bool {
	return envelopeSize <= RawCodecThresholdBytes
}
