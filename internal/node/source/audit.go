package source

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/state"
)

const maxAuditBody = 4 << 10 // challenge JSON is ~300 bytes

// handleAuditChallenge serves a synchronous block_hash possession challenge.
// Stricter than handleBlob: COORDINATOR ONLY, NO repair token; assignment-bound;
// returns the challenged block's bytes (the coordinator verifies by CID
// reconstruction). 404 is the clean "I do not hold it / stale assignment" signal;
// 429 means the audit governor is exhausted (the coordinator records a skip).
func (s *Server) handleAuditChallenge(w http.ResponseWriter, r *http.Request) {
	const codeUnauthorized, codeBlobUnavail, codeBudget, codeBad = "audit_unauthorized", "blob_unavailable", "budget_exceeded", "bad_request"
	now := s.now()

	// 1) Coordinator ONLY (audits are control traffic; reject RoleNode).
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		s.refuse(w, http.StatusForbidden, codeUnauthorized, "no_peer_cert", "")
		return
	}
	peer, err := transport.IdentityFromCert(r.TLS.PeerCertificates[0])
	if err != nil || peer.Role != transport.RoleCoordinator {
		s.refuse(w, http.StatusForbidden, codeUnauthorized, "wrong_role", "")
		return
	}

	// 2) Parse the bounded challenge body.
	var ch wire.AuditChallenge
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAuditBody)).Decode(&ch); err != nil ||
		ch.ChallengeKind != wire.AuditChallengeKindBlockHash || ch.BlobCID == "" || ch.BlockCID == "" {
		s.refuse(w, http.StatusBadRequest, codeBad, "bad_challenge", ch.BlobCID)
		return
	}
	// 2a) Local block-size ceiling (defense against a buggy/malicious coordinator
	// requesting a huge read): reject BEFORE any budget debit or block read.
	if ch.BlockSize <= 0 || ch.BlockSize > s.maxAuditBlockBytes {
		s.refuse(w, http.StatusBadRequest, codeBad, "block_size_out_of_range", ch.BlobCID)
		return
	}

	// 3) Assignment-bound: acked-delivered + assignment/generation match (D-M6-4-BIND).
	prog, ok := s.progress.Get(ch.BlobCID)
	if !ok || prog.State != state.ProgressAckDelivered ||
		prog.AssignmentID != ch.AssignmentID || prog.Generation != ch.Generation {
		s.refuse(w, http.StatusNotFound, codeBlobUnavail, "progress_mismatch", ch.BlobCID)
		return
	}

	// 4) Recursive PIN (not stray block residue).
	has, err := s.pinner.Has(r.Context(), ch.BlobCID)
	if err != nil || !has {
		s.refuse(w, http.StatusNotFound, codeBlobUnavail, "not_pinned", ch.BlobCID)
		return
	}

	// 5) Audit governor (separate from the M5 source/repair bucket, D-M6-6).
	if !s.auditBudget.Take(ch.BlockSize, now) {
		s.refuse(w, http.StatusTooManyRequests, codeBudget, "audit_budget", ch.BlobCID)
		return
	}

	// 6) Local-only block read; ANY error (incl. not-present) is a clean 404. We do
	// not import the concrete ipfsclient here — the package stays interface-only.
	block, err := s.auditBlocks.BlockGetLocal(r.Context(), ch.BlockCID)
	if err != nil {
		s.refuse(w, http.StatusNotFound, codeBlobUnavail, "block_unavailable", ch.BlobCID)
		return
	}
	if int64(len(block)) != ch.BlockSize {
		s.refuse(w, http.StatusNotFound, codeBlobUnavail, "block_size_mismatch", ch.BlobCID)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(block)
	slog.Info("node.audit.served", "blob", ch.BlobCID, "block", ch.BlockCID, "bytes", len(block))
}
