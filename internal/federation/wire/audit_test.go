package wire

import (
	"bytes"
	"testing"
)

func TestAuditTranscriptHashLengthPrefixed(t *testing.T) {
	// Length-prefixing must make ("ab","c") != ("a","bc") even though raw concat is equal.
	base := AuditChallenge{ChallengeID: "id", BlobCID: "blob", BlockCID: "blk", BlockIndex: 3, Nonce: "n"}
	h1 := AuditTranscriptHash(AuditChallenge{ChallengeID: "ab", BlobCID: base.BlobCID, BlockCID: base.BlockCID, Nonce: base.Nonce}, []byte("x"))
	h2 := AuditTranscriptHash(AuditChallenge{ChallengeID: "a", BlobCID: "b" + base.BlobCID, BlockCID: base.BlockCID, Nonce: base.Nonce}, []byte("x"))
	if bytes.Equal(h1, h2) {
		t.Fatal("length-prefixing failed: ambiguous concat")
	}
	if len(AuditTranscriptHash(base, []byte("x"))) != 32 {
		t.Fatal("want sha256 length")
	}
}
