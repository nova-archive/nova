// Package source is the donor's INBOUND read-source server (P2-M4.1, D-M4.1-3):
// an mTLS GET /fed/v1/blob/{cid} handler that serves a pinned ciphertext
// envelope to the AUTHENTICATED COORDINATOR ONLY. Every request runs a strict
// assignment-bound verify chain (role + signed read-grant + binding + local
// state + boot-floor + single-use replay + pin) before the FIRST real D11
// egress-budget debit (no refund) and a size-capped stream. Donors serve only
// ciphertext; the coordinator verifies+decrypts, so the donor never sees
// plaintext.
//
// The package is built around small INJECTABLE interfaces so the handler is
// unit-testable with fakes (no real Kubo, no real TLS handshake). It is pure
// stdlib + the dependency-free wire/transport/replay/state packages, so it
// stays inside the donor dependency boundary (cmd/node must pass
// scripts/check_node_deps.sh — replay is the one reviewed allowlist addition).
package source

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nova-archive/nova/internal/federation/replay"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/state"
)

// Pinner is the local pin store the donor reads from. Satisfied by
// *ipfsclient.Client.
type Pinner interface {
	Has(ctx context.Context, cid string) (bool, error)
	Get(ctx context.Context, cid string) (io.ReadCloser, error)
}

// Budget is the authoritative donor egress enforcer (D11). Satisfied by
// *bandwidth.Bucket. Take debits n bytes as of now and returns false when the
// daily budget cannot cover the request (no refund on a later stream abort).
type Budget interface {
	Take(n int64, now time.Time) bool
}

// PubKeyProvider supplies the CURRENT coordinator repair-token public key
// (D-M4.1-18). It returns ok=false until a heartbeat has delivered one, which
// the handler treats as fail-closed (503 source_unavailable). Satisfied by
// *KeyProvider.
type PubKeyProvider interface {
	Current() (ed25519.PublicKey, bool)
}

// ProgressLookup is the donor's local fetch/verify/ack record per CID.
// Satisfied by *state.FileProgressStore.
type ProgressLookup interface {
	Get(cid string) (state.Progress, bool)
}

// Deps are the injected collaborators + identity for a Server.
type Deps struct {
	Pinner      Pinner
	Budget      Budget
	PubKey      PubKeyProvider
	Progress    ProgressLookup
	NodeID      string // this donor's own node_id; read-grants must name it as source
	BootTime    time.Time
	ReplayCache *replay.Cache
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// Server is the donor read-source HTTP handler.
type Server struct {
	pinner   Pinner
	budget   Budget
	pubkey   PubKeyProvider
	progress ProgressLookup
	nodeID   string
	boot     time.Time
	replay   *replay.Cache
	now      func() time.Time
	mux      *http.ServeMux
}

// NewServer wires a Server from its deps and registers the route.
func NewServer(d Deps) *Server {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	s := &Server{
		pinner:   d.Pinner,
		budget:   d.Budget,
		pubkey:   d.PubKey,
		progress: d.Progress,
		nodeID:   d.NodeID,
		boot:     d.BootTime,
		replay:   d.ReplayCache,
		now:      now,
		mux:      http.NewServeMux(),
	}
	// Go 1.22 method+path routing; no chi.
	s.mux.HandleFunc("GET /fed/v1/blob/{cid}", s.handleBlob)
	return s
}

// ServeHTTP lets Server back an http.Server directly.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// refuse writes the normalized error envelope and logs the refusal reason.
func (s *Server) refuse(w http.ResponseWriter, status int, code, reason, cid string) {
	slog.Info("node.source.refused", "reason", reason, "code", code, "cid", cid)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(wire.ErrorResponse{Code: code})
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	const (
		codeUnauthorized = "source_unauthorized"
		codeUnavailable  = "source_unavailable"
		codeBlobUnavail  = "blob_unavailable"
		codeTooLarge     = "blob_too_large"
		codeBudget       = "budget_exceeded"
	)
	now := s.now()
	cid := r.PathValue("cid")

	// 1) Caller must be the authenticated coordinator. The mTLS listener
	// already REQUIRED+VERIFIED the client cert against the federation CA; we
	// only read the leaf's role here.
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		s.refuse(w, http.StatusForbidden, codeUnauthorized, "no_peer_cert", cid)
		return
	}
	peer, err := transport.IdentityFromCert(r.TLS.PeerCertificates[0])
	if err != nil || (peer.Role != transport.RoleCoordinator && peer.Role != transport.RoleNode) {
		s.refuse(w, http.StatusForbidden, codeUnauthorized, "wrong_role", cid)
		return
	}

	// 2) Coordinator repair pubkey must be known (fail-closed, D-M4.1-18).
	pub, ok := s.pubkey.Current()
	if !ok {
		s.refuse(w, http.StatusServiceUnavailable, codeUnavailable, "no_pubkey", cid)
		return
	}

	// 3) Verify the signed read-grant (signature + structure + time window).
	claims, err := wire.Verify(pub, r.Header.Get("X-Nova-Repair-Token"), now)
	if err != nil {
		s.refuse(w, http.StatusForbidden, codeUnauthorized, "token_verify", cid)
		return
	}

	// 4) Binding: this donor is the named source, the requester is the named dest,
	// and the CID matches the path (defends against token reuse across CIDs). A
	// coordinator caller (M4.1 read) must be bound as the synthetic coordinator
	// dest; a donor caller (M5 repair) must be bound as its own verified node id —
	// so a grant minted for donor B cannot be replayed by donor C.
	if claims.SourceNodeID != s.nodeID {
		s.refuse(w, http.StatusForbidden, codeUnauthorized, "source_mismatch", cid)
		return
	}
	wantDest := wire.CoordinatorSourceID
	if peer.Role == transport.RoleNode {
		wantDest = peer.NodeID
	}
	if claims.DestNodeID != wantDest {
		s.refuse(w, http.StatusForbidden, codeUnauthorized, "dest_mismatch", cid)
		return
	}
	if claims.CID != cid {
		s.refuse(w, http.StatusForbidden, codeUnauthorized, "cid_mismatch", cid)
		return
	}

	// 5) Local state must show this exact assignment generation acked-delivered.
	prog, ok := s.progress.Get(cid)
	if !ok || prog.State != state.ProgressAckDelivered ||
		prog.AssignmentID != claims.AssignmentID || prog.Generation != claims.Generation {
		s.refuse(w, http.StatusNotFound, codeBlobUnavail, "progress_mismatch", cid)
		return
	}

	// 6) Restart-safe replay defense (D-M4-9): the grant must be minted at or
	// after this server's boot, and its JTI single-use.
	if !replay.BootFloorOK(claims.NotBefore, s.boot) {
		s.refuse(w, http.StatusForbidden, codeUnauthorized, "pre_boot", cid)
		return
	}
	if !s.replay.UseOnce(claims.JTI, time.Unix(claims.NotAfter, 0), now) {
		s.refuse(w, http.StatusForbidden, codeUnauthorized, "replay", cid)
		return
	}

	// 7) The blob must actually be pinned here.
	has, err := s.pinner.Has(r.Context(), cid)
	if err != nil || !has {
		s.refuse(w, http.StatusNotFound, codeBlobUnavail, "not_pinned", cid)
		return
	}

	// 8) Preflight size (D-M4.1-16): use the recorded ENVELOPE size; refuse
	// (never truncate) anything over the grant's max_bytes, BEFORE opening any
	// body byte.
	size := prog.ByteSize
	if size <= 0 {
		s.refuse(w, http.StatusNotFound, codeBlobUnavail, "no_size", cid)
		return
	}
	if size > claims.MaxBytes {
		s.refuse(w, http.StatusRequestEntityTooLarge, codeTooLarge, "too_large", cid)
		return
	}

	// 9) FIRST real D11 egress debit (no refund). Reserve BEFORE opening body.
	if !s.budget.Take(size, now) {
		s.refuse(w, http.StatusTooManyRequests, codeBudget, "budget", cid)
		return
	}

	// 10) Stream EXACTLY `size` bytes (D-M5-8d): CopyN never writes more than the
	// recorded envelope size, so a donor whose pinned object drifted larger cannot
	// put extra bytes on the wire (the dest's re-import CID verify is still the
	// backstop, but "stream exactly size" is now true). A short pinned object
	// yields fewer than `size` bytes and an error, which the dest also catches via
	// CID mismatch — either way the dest never acks a wrong blob.
	rc, err := s.pinner.Get(r.Context(), cid)
	if err != nil {
		s.refuse(w, http.StatusNotFound, codeBlobUnavail, "get_failed", cid)
		return
	}
	defer rc.Close()

	start := now
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	copied, err := io.CopyN(w, rc, size)
	if err != nil {
		// Short read: the pinned object is smaller than the recorded size. The
		// header is already sent, so we cannot change the status; log the drift.
		// The dest's CID verify rejects the truncated body (no ack).
		slog.Error("node.source.short_read", "cid", cid, "recorded", size, "copied", copied, "err", err)
		return
	}
	slog.Info("node.source.served",
		"cid", cid, "bytes", copied,
		"dur_ms", time.Since(start).Milliseconds(),
		"dest", peer.NodeID)
}

// KeyProvider is a fail-closed "current coordinator repair pubkey" holder
// (D-M4.1-18). The donor agent calls Set on every heartbeat that carries
// HeartbeatResponse.RepairTokenPublicKey; the source server reads Current.
//
// Rotation choice: this is a CURRENT-KEY-ONLY provider — Set overwrites the
// previous key. The coordinator delivers the active pubkey on EVERY heartbeat
// (D-M4-7), so a key change converges to the new key within one heartbeat
// interval, and the boot-floor + short read-grant TTL already bound any replay
// window. We deliberately do NOT keep a previous-key grace window: it would
// widen the window in which a compromised retired key still verifies grants,
// for no availability gain over "fail-closed until the next heartbeat". A grant
// minted under the old key during the brief skew is simply refused (the
// coordinator re-mints under the new key on retry).
type KeyProvider struct {
	mu  sync.RWMutex
	pub ed25519.PublicKey
}

// Set installs pub as the current key (overwriting any previous key).
func (k *KeyProvider) Set(pub ed25519.PublicKey) {
	k.mu.Lock()
	k.pub = pub
	k.mu.Unlock()
}

// Current returns the installed key, ok=false when none has been set yet.
func (k *KeyProvider) Current() (ed25519.PublicKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.pub == nil {
		return nil, false
	}
	return k.pub, true
}
