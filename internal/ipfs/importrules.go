package ipfs

import "github.com/nova-archive/nova/internal/ipfs/importspec"

// Deterministic import params now live in the dependency-light importspec
// sub-package (so the donor can share them without embedded Kubo). Re-exported
// here for the coordinator's existing call sites.
const (
	RawCodecThresholdBytes = importspec.RawCodecThresholdBytes
	ChunkerSizeBytes       = importspec.ChunkerSizeBytes
	ChunkerSpec            = importspec.ChunkerSpec
	HashAlg                = importspec.HashAlg
	CodecRaw               = importspec.CodecRaw
	CodecDagPB             = importspec.CodecDagPB
	MaxLinkCount           = importspec.MaxLinkCount
)

// ShouldUseRawCodec is re-exported from importspec.
func ShouldUseRawCodec(envelopeSize int64) bool { return importspec.ShouldUseRawCodec(envelopeSize) }
