package importspec_test

import (
	"testing"
	"github.com/nova-archive/nova/internal/ipfs/importspec"
)

func TestImportSpecValues(t *testing.T) {
	if importspec.ChunkerSpec != "size-262144" || importspec.ChunkerSizeBytes != 262144 {
		t.Fatal("chunker drift")
	}
	if importspec.RawCodecThresholdBytes != 1<<20 {
		t.Fatal("threshold drift")
	}
	if !importspec.ShouldUseRawCodec(1<<20) || importspec.ShouldUseRawCodec((1<<20)+1) {
		t.Fatal("raw threshold boundary wrong")
	}
	if importspec.HashAlg != "sha2-256" || importspec.CodecRaw != "raw" || importspec.CodecDagPB != "dag-pb" {
		t.Fatal("codec/hash drift")
	}
}
