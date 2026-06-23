package coordinator

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	gocid "github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// jtiCache is an in-memory single-use replay cache for repair-token jti values
// (D-M4-9). Entries expire at the token's not_after; combined with the
// source_boot_time floor, restart leaves no usable replay window.
type jtiCache struct {
	mu   sync.Mutex
	seen map[string]time.Time // jti -> expiry
}

func newJTICache() *jtiCache { return &jtiCache{seen: map[string]time.Time{}} }

// useOnce returns false if jti was already seen (replay). It also opportunistically
// sweeps expired entries.
func (c *jtiCache) useOnce(jti string, exp time.Time, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.seen {
		if now.After(e) {
			delete(c.seen, k)
		}
	}
	if _, ok := c.seen[jti]; ok {
		return false
	}
	c.seen[jti] = exp
	return true
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	node, ok := s.authNode(w, r)
	if !ok {
		return
	}
	if s.signer == nil || s.backend == nil {
		writeError(w, http.StatusServiceUnavailable, "source_unavailable", "")
		return
	}
	cidStr := r.PathValue("cid")
	c, err := gocid.Decode(cidStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_cid", "")
		return
	}
	now := time.Now()

	tok := r.Header.Get("X-Nova-Repair-Token")
	pub, err := wire.DecodePublicKey(s.signer.PublicKeyWire())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "pubkey")
		return
	}
	claims, err := wire.Verify(pub, tok, now)
	if err != nil {
		slog.Info("fed.token.rejected", "reason", "verify", "err", err)
		writeError(w, http.StatusForbidden, wire.FailReasonSourceUnauthorized, "token verify failed")
		return
	}
	// Bindings: source is us, dest is the caller, cid matches the path.
	if claims.SourceNodeID != tokens.ReservedCoordinatorSourceID || claims.DestNodeID != node.String() || claims.CID != cidStr {
		slog.Info("fed.token.rejected", "reason", "binding", "cid", cidStr)
		writeError(w, http.StatusForbidden, wire.FailReasonSourceUnauthorized, "token binding mismatch")
		return
	}
	// Restart-safe replay defense (D-M4-9).
	if claims.NotBefore < s.sourceBootTime.Unix() {
		slog.Info("fed.token.rejected", "reason", "pre_boot")
		writeError(w, http.StatusForbidden, wire.FailReasonSourceUnauthorized, "token predates source boot")
		return
	}
	if !s.jti.useOnce(claims.JTI, time.Unix(claims.NotAfter, 0), now) {
		slog.Info("fed.token.rejected", "reason", "replay", "jti", claims.JTI)
		writeError(w, http.StatusForbidden, wire.FailReasonSourceUnauthorized, "token already used")
		return
	}

	// Preflight size (D-M4-3): reject before writing any body byte.
	size, err := s.q.GetBlobByteSize(r.Context(), cidStr)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, wire.FailReasonBlobUnavailable, "")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "size")
		return
	}
	maxBytes := claims.MaxBytes
	if s.cfg.MaxTransferBytes > 0 && s.cfg.MaxTransferBytes < maxBytes {
		maxBytes = s.cfg.MaxTransferBytes
	}
	if size > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "blob_too_large", "")
		return
	}

	rc, err := s.backend.Get(r.Context(), c)
	if err != nil {
		writeError(w, http.StatusNotFound, wire.FailReasonBlobUnavailable, "")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	n, _ := io.Copy(w, io.LimitReader(rc, maxBytes))
	slog.Info("fed.blob.served", "cid", cidStr, "bytes", n, "dest_node_id", node, "jti", claims.JTI)
}
