// Package importspec holds the deterministic IPFS import parameters
// (IPFS_IMPORT_RULES.md, Tier-1). It is dependency-free so BOTH the operator's
// embedded Kubo (internal/ipfs) and the donor's Kubo-sidecar client
// (internal/node/ipfsclient) share identical params and therefore produce
// bit-identical root CIDs. It MUST NOT import Kubo or any operator-only package.
package importspec

const (
	RawCodecThresholdBytes int64 = 1 << 20 // 1 MiB
	ChunkerSizeBytes       int64 = 262144  // 256 KiB
	ChunkerSpec                  = "size-262144"
	HashAlg                      = "sha2-256"
	CodecRaw                     = "raw"
	CodecDagPB                   = "dag-pb"
	MaxLinkCount                 = 174
)

// ShouldUseRawCodec reports whether an envelope of envelopeSize bytes imports via
// the single-block raw-codec shortcut (true) or the dag-pb chunked path (false).
func ShouldUseRawCodec(envelopeSize int64) bool { return envelopeSize <= RawCodecThresholdBytes }
