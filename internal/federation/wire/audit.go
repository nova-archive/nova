package wire

import (
	"crypto/sha256"
	"encoding/binary"
)

// AuditChallenge is the coordinator→donor possession-audit request body
// (challenge_kind == "block_hash"). assignment_id/generation make it
// assignment-bound (D-M6-4-BIND); block_size lets the donor length-check.
type AuditChallenge struct {
	ChallengeID   string `json:"challenge_id"`
	ChallengeKind string `json:"challenge_kind"`
	BlobCID       string `json:"blob_cid"`
	AssignmentID  string `json:"assignment_id"`
	Generation    int64  `json:"generation"`
	BlockIndex    int64  `json:"block_index"`
	BlockCID      string `json:"block_cid"`
	BlockSize     int64  `json:"block_size"`
	Nonce         string `json:"nonce"`
}

// AuditChallengeKindBlockHash is the only kind shipped in M6.
const AuditChallengeKindBlockHash = "block_hash"

const auditTranscriptDomain = "NOVA-POSSESSION-AUDIT-v1"

// AuditTranscriptHash is the domain-separated, length-prefixed audit transcript
// digest (D-M6-3a). CID reconstruction is the primary verifier; this digest is
// the durable transcript / test-vector artifact stored in pin_audits.transcript_hash.
func AuditTranscriptHash(c AuditChallenge, blockBytes []byte) []byte {
	h := sha256.New()
	h.Write([]byte(auditTranscriptDomain))
	h.Write([]byte{0x00})
	lp := func(b []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	be64 := func(v int64) { var b [8]byte; binary.BigEndian.PutUint64(b[:], uint64(v)); h.Write(b[:]) }
	lp([]byte(c.ChallengeID))
	lp([]byte(c.BlobCID))
	lp([]byte(c.AssignmentID))
	be64(c.Generation)
	lp([]byte(c.BlockCID))
	be64(c.BlockIndex)
	be64(c.BlockSize)
	lp([]byte(c.Nonce))
	lp(blockBytes)
	return h.Sum(nil)
}
