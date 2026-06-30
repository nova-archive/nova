package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/wire"
)

const defaultPinPageLimit = 1000

// authNode authenticates the peer, parses its node UUID, and rejects nodes that
// are out of the federation (revoked, evicted) — the shared front-half of every
// /fed/v1/pins/* handler. It returns the node's status so per-endpoint matrices
// (D-M5-5) can apply finer rules (e.g. changes pauses for unreachable nodes).
func (s *Server) authNode(w http.ResponseWriter, r *http.Request) (uuid.UUID, gen.NodeStatus, bool) {
	id, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return uuid.Nil, "", false
	}
	nodeUUID, err := uuid.Parse(id.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_node_id", "")
		return uuid.Nil, "", false
	}
	node, err := s.q.GetNodeByID(r.Context(), pgUUIDFrom(nodeUUID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "registration_required", "node must register first")
		return uuid.Nil, "", false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "lookup failed")
		return uuid.Nil, "", false
	}
	if node.Status == gen.NodeStatusRevoked {
		writeError(w, http.StatusForbidden, "node_revoked", "")
		return uuid.Nil, "", false
	}
	if node.Status == gen.NodeStatusEvicted {
		writeError(w, http.StatusForbidden, "registration_required", "evicted node must re-register")
		return uuid.Nil, "", false
	}
	if node.FederationCertFingerprint != id.Fingerprint {
		writeError(w, http.StatusForbidden, "fingerprint_mismatch", "presented cert is not the active cert")
		return uuid.Nil, "", false
	}
	return nodeUUID, node.Status, true
}

// queryInt parses a non-negative int64 query param. Absent ⇒ def. Present but
// malformed or negative ⇒ error (the caller returns 400 bad_request).
func queryInt(r *http.Request, key string, def int64) (int64, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return n, nil
}

func clampLimit(n int64) int32 {
	if n < 1 || n > defaultPinPageLimit {
		return defaultPinPageLimit
	}
	return int32(n)
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	node, status, ok := s.authNode(w, r)
	if !ok {
		return
	}
	// An unreachable node must heartbeat to reactivate (which resets it to
	// 'reconciling') before pulling changes — otherwise it would resync as a
	// still-uncounted holder without the recovery transition (D-M5-5 matrix).
	if status == gen.NodeStatusUnreachable {
		writeError(w, http.StatusForbidden, "heartbeat_required", "unreachable node must heartbeat to reactivate")
		return
	}
	ctx := r.Context()
	since, err := queryInt(r, "since_seq", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	limRaw, err := queryInt(r, "limit", defaultPinPageLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	limit := clampLimit(limRaw)

	wm, err := s.q.GetPruneWatermark(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "watermark")
		return
	}
	if since < wm {
		// Cursor predates retention: the node cannot catch up incrementally and
		// must snapshot (D-M5-5 sync-state table).
		s.setSyncState(ctx, node, "snapshot_required")
		writeError(w, http.StatusBadRequest, wire.CodeSnapshotRequired, "since_seq predates retention")
		return
	}
	rows, err := s.q.GetPinChangesSince(ctx, gen.GetPinChangesSinceParams{
		NodeID: pgUUIDFrom(node), Sequence: since, Limit: limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "changes")
		return
	}
	head, err := s.q.GetChangeLogHead(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "head")
		return
	}
	now := time.Now()
	changes := make([]wire.PinChange, len(rows))
	for i, row := range rows {
		changes[i] = wire.PinChange{
			Sequence: row.Sequence, AssignmentID: uuid.UUID(row.AssignmentID.Bytes).String(),
			Generation: row.Generation, Kind: row.Kind, CID: row.Cid, ByteSize: row.ByteSize,
		}
		if row.Kind == wire.ChangeKindAssign && s.signer != nil {
			if src := s.mintForChange(ctx, changes[i], row.SourceNodeID, node, now); src != nil {
				changes[i].Source = src
			}
		}
	}
	// next_seq must never exceed a row we actually delivered. Advancing to the
	// global head would skip a change that committed for this node between the page
	// read and the head read (the page misses it; head includes it) — silent
	// assignment loss. Advance only to the highest delivered sequence; when the
	// page is empty, stay at `since` and let the donor re-poll.
	next := since
	if len(rows) > 0 {
		next = rows[len(rows)-1].Sequence
	}
	// A short page (fewer rows than requested) means the node has drained its
	// change stream — it is caught up to the current epoch, so it counts again
	// (D-M5-2a/4a). A full page may have more pending, so it stays mid-sync.
	if len(rows) < int(limit) {
		s.setSyncState(ctx, node, "current")
	}
	slog.Info("fed.changes.served", "node_id", node, "since_seq", since, "returned", len(rows), "next_seq", next)
	writeJSON(w, http.StatusOK, wire.ChangesResponse{Changes: changes, NextSeq: next, CurrentEpoch: head})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	node, _, ok := s.authNode(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cursor := r.URL.Query().Get("cursor")
	limRaw, err := queryInt(r, "limit", defaultPinPageLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	limit := clampLimit(limRaw)

	head, err := s.q.GetChangeLogHead(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "head")
		return
	}
	epoch := head // first page captures the global head
	if ep := r.URL.Query().Get("snapshot_epoch"); ep != "" {
		epoch, err = queryInt(r, "snapshot_epoch", head)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		changed, err := s.q.NodeHasChangesAfter(ctx, gen.NodeHasChangesAfterParams{NodeID: pgUUIDFrom(node), Sequence: epoch})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "epoch-check")
			return
		}
		if changed {
			slog.Info("fed.snapshot.conflict", "node_id", node, "epoch", epoch)
			writeJSON(w, http.StatusConflict, map[string]any{"code": "snapshot_epoch_changed", "snapshot_epoch": head})
			return
		}
	}
	rows, err := s.q.GetPinSnapshotPage(ctx, gen.GetPinSnapshotPageParams{NodeID: pgUUIDFrom(node), Cid: cursor, Limit: int32(limit)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "snapshot")
		return
	}
	now := time.Now()
	items := make([]wire.SnapshotItem, len(rows))
	for i, row := range rows {
		items[i] = wire.SnapshotItem{
			CID: row.Cid, AssignmentID: uuid.UUID(row.AssignmentID.Bytes).String(),
			Generation: row.Generation, ByteSize: row.ByteSize, AssignedAt: row.AssignedAt.Format(time.RFC3339),
		}
		// Snapshot recovery must learn the repair source too (D-M5-8a): a donor that
		// recovers via snapshot after change-log retention would otherwise never know
		// its source. Reuse the same late-mint as /pins/changes via a synthetic change.
		if s.signer != nil {
			synth := wire.PinChange{AssignmentID: items[i].AssignmentID, Generation: row.Generation, CID: row.Cid, ByteSize: row.ByteSize}
			if src := s.mintForChange(ctx, synth, row.SourceNodeID, node, now); src != nil {
				items[i].Source = src
			}
		}
	}
	nextCursor := ""
	if int64(len(rows)) == int64(limit) && len(rows) > 0 {
		nextCursor = rows[len(rows)-1].Cid
	}
	// The final page (cursor exhausted) means the node has the full desired set as
	// of the captured epoch, so it counts again (D-M5-5 sync-state table).
	if nextCursor == "" {
		s.setSyncState(ctx, node, "current")
	}
	slog.Info("fed.snapshot.page", "node_id", node, "epoch", epoch, "returned", len(rows))
	writeJSON(w, http.StatusOK, wire.SnapshotResponse{Data: items, Cursor: nextCursor, SnapshotEpoch: epoch})
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	// ack/fail accept active/suspect/unreachable (a recovering node may still be
	// reporting); authNode already rejects evicted/revoked (D-M5-5 matrix).
	node, _, ok := s.authNode(w, r)
	if !ok {
		return
	}
	cid := r.PathValue("cid")
	var req wire.Ack
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "malformed ack")
		return
	}
	if req.CID != "" && req.CID != cid {
		writeError(w, http.StatusBadRequest, "cid_mismatch", "body cid does not match path")
		return
	}
	aid, err := uuid.Parse(req.AssignmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_assignment_id", "")
		return
	}
	ctx := r.Context()
	n, err := s.q.AckPinAssignment(ctx, gen.AckPinAssignmentParams{
		Cid: cid, NodeID: pgUUIDFrom(node), AssignmentID: pgUUIDFrom(aid), Generation: req.Generation,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "ack")
		return
	}
	if n == 1 {
		slog.Info("fed.ack.applied", "cid", cid, "assignment_id", req.AssignmentID, "generation", req.Generation)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// 0 rows: idempotent replay vs stale vs unknown
	cur, err := s.q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{Cid: cid, NodeID: pgUUIDFrom(node)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "unknown_assignment", "")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "ack-lookup")
		return
	}
	if uuid.UUID(cur.AssignmentID.Bytes) == aid && cur.Generation == req.Generation && cur.State == gen.PinStateAcked {
		w.WriteHeader(http.StatusNoContent) // idempotent
		return
	}
	slog.Info("fed.ack.stale", "cid", cid, "assignment_id", req.AssignmentID, "generation", req.Generation)
	writeError(w, http.StatusConflict, wire.CodeStaleAssignment, "")
}

func (s *Server) handleFail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	node, _, ok := s.authNode(w, r)
	if !ok {
		return
	}
	cid := r.PathValue("cid")
	var req wire.Fail
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "malformed fail")
		return
	}
	if req.CID != "" && req.CID != cid {
		writeError(w, http.StatusBadRequest, "cid_mismatch", "body cid does not match path")
		return
	}
	if req.Reason = wire.NormalizeFailReason(req.Reason); req.Reason == "" {
		writeError(w, http.StatusBadRequest, "bad_reason", "unknown fail reason")
		return
	}
	aid, err := uuid.Parse(req.AssignmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_assignment_id", "")
		return
	}
	ctx := r.Context()
	n, err := s.q.FailPinAssignment(ctx, gen.FailPinAssignmentParams{
		Cid: cid, NodeID: pgUUIDFrom(node), AssignmentID: pgUUIDFrom(aid), Generation: req.Generation,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "fail")
		return
	}
	if n == 1 {
		slog.Info("fed.fail.applied", "cid", cid, "reason", req.Reason, "generation", req.Generation)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cur, err := s.q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{Cid: cid, NodeID: pgUUIDFrom(node)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "unknown_assignment", "")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "fail-lookup")
		return
	}
	if uuid.UUID(cur.AssignmentID.Bytes) == aid && cur.Generation == req.Generation && cur.State == gen.PinStateFailed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeError(w, http.StatusConflict, wire.CodeStaleAssignment, "")
}

// setSyncState advances a node's assignment_sync_state during recovery. It is
// advisory: a failure is logged but never fails the response the node already
// received — the next successful poll/snapshot re-derives the state (D-M5-2a).
func (s *Server) setSyncState(ctx context.Context, node uuid.UUID, state string) {
	if err := s.q.SetNodeSyncState(ctx, gen.SetNodeSyncStateParams{ID: pgUUIDFrom(node), AssignmentSyncState: state}); err != nil {
		slog.Warn("fed.syncstate.update_failed", "node_id", node, "state", state, "err", err)
	}
}

// mintForChange builds the repair-source grant for an assign change (D-M5-8a). A
// NULL stored source_node_id ⇒ coordinator-as-source (the D-M5-8b emergency path
// also lands here, since a local-recoverable CID with no donor source is scheduled
// with a NULL source); a non-NULL source ⇒ donor-as-source, late-bound to the
// source's CURRENT address.
func (s *Server) mintForChange(ctx context.Context, ch wire.PinChange, storedSource pgtype.UUID, dest uuid.UUID, now time.Time) *wire.ChangeSource {
	if !storedSource.Valid {
		return s.mintSource(ch, dest, now)
	}
	return s.mintDonorSource(ctx, ch, storedSource, dest, now)
}

// mintDonorSource resolves the donor source's current address + acked assignment
// and mints a donor↔donor grant whose Source claim names the SOURCE's acked
// assignment (D-M5-8e) and whose Dest* fields bind THIS pending assignment. If the
// stored source is no longer repair-sourceable, it requeues with backoff (never
// silently substitutes) and returns nil for this serve — the scheduler re-picks
// once the backoff elapses (D-M5-8a).
func (s *Server) mintDonorSource(ctx context.Context, ch wire.PinChange, storedSource pgtype.UUID, dest uuid.UUID, now time.Time) *wire.ChangeSource {
	if s.cfg.RepairTokenTTL <= 0 {
		return nil
	}
	src, err := s.q.GetRepairSource(ctx, gen.GetRepairSourceParams{ID: storedSource, Cid: ch.CID})
	if errors.Is(err, pgx.ErrNoRows) {
		if rqErr := s.q.RequeuePinAssignmentSource(ctx, gen.RequeuePinAssignmentSourceParams{
			Cid: ch.CID, NodeID: pgUUIDFrom(dest),
		}); rqErr != nil {
			slog.Warn("fed.repair.requeue_failed", "cid", ch.CID, "err", rqErr)
		}
		slog.Info("fed.repair.source_unsourceable_requeued", "cid", ch.CID, "dest", dest)
		return nil
	}
	if err != nil {
		slog.Warn("fed.repair.source_lookup_failed", "cid", ch.CID, "err", err)
		return nil
	}
	jti, err := uuid.NewRandom()
	if err != nil {
		return nil
	}
	// Donor source: clamp NotBefore only to now-skew. The SOURCE donor enforces its
	// OWN boot floor; the coordinator's sourceBootTime is irrelevant for a donor grant.
	nb := now.Add(-5 * time.Second)
	sourceID := uuid.UUID(storedSource.Bytes).String()
	tok, err := s.signer.Mint(wire.Claims{
		JTI:              jti.String(),
		AssignmentID:     uuid.UUID(src.AssignmentID.Bytes).String(), // SOURCE's acked assignment
		Generation:       src.Generation,
		CID:              ch.CID,
		SourceNodeID:     sourceID,
		DestNodeID:       dest.String(),
		NotBefore:        nb.Unix(),
		NotAfter:         now.Add(s.cfg.RepairTokenTTL).Unix(),
		MaxBytes:         ch.ByteSize,
		ProtocolVersion:  wire.ProtocolV1,
		DestAssignmentID: ch.AssignmentID, // THIS (destination's) pending assignment
		DestGeneration:   ch.Generation,
	})
	if err != nil {
		slog.Warn("fed.repair.mint_failed", "cid", ch.CID, "err", err)
		return nil
	}
	slog.Info("fed.repair.minted", "cid", ch.CID, "source", sourceID, "dest", dest)
	return &wire.ChangeSource{NodeID: sourceID, NebulaAddr: src.SourceNebulaAddr.String, Token: tok}
}

// mintSource builds a fresh coordinator-as-source grant for a pending assign
// (D-M4-8). Tokens are minted per-serve and NEVER persisted in pin_changes.
func (s *Server) mintSource(ch wire.PinChange, dest uuid.UUID, now time.Time) *wire.ChangeSource {
	if s.cfg.RepairTokenTTL <= 0 {
		slog.Warn("fed.token.mint_skipped", "reason", "non_positive_ttl", "cid", ch.CID)
		return nil
	}
	jti, err := uuid.NewRandom()
	if err != nil {
		return nil
	}
	// NotBefore is now-skew, but NEVER earlier than sourceBootTime — otherwise the
	// blob endpoint's pre-boot replay floor (D-M4-9) would reject a token we just
	// minted right after startup.
	nb := now.Add(-5 * time.Second)
	if nb.Before(s.sourceBootTime) {
		nb = s.sourceBootTime
	}
	tok, err := s.signer.Mint(wire.Claims{
		JTI: jti.String(), AssignmentID: ch.AssignmentID, Generation: ch.Generation,
		CID: ch.CID, SourceNodeID: tokens.ReservedCoordinatorSourceID, DestNodeID: dest.String(),
		NotBefore: nb.Unix(),
		NotAfter:  now.Add(s.cfg.RepairTokenTTL).Unix(),
		MaxBytes:  ch.ByteSize, ProtocolVersion: wire.ProtocolV1,
	})
	if err != nil {
		slog.Warn("fed.token.mint_failed", "cid", ch.CID, "err", err)
		return nil
	}
	slog.Info("fed.token.minted", "assignment_id", ch.AssignmentID, "cid", ch.CID, "dest_node_id", dest)
	return &wire.ChangeSource{NodeID: tokens.ReservedCoordinatorSourceID, NebulaAddr: s.cfg.SourceNebulaAddr, Token: tok}
}
