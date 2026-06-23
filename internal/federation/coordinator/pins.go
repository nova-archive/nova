package coordinator

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
)

const defaultPinPageLimit = 1000

// authNode authenticates the peer, parses its node UUID, and rejects revoked
// nodes — the shared front-half of every /fed/v1/pins/* handler.
func (s *Server) authNode(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return uuid.Nil, false
	}
	nodeUUID, err := uuid.Parse(id.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_node_id", "")
		return uuid.Nil, false
	}
	node, err := s.q.GetNodeByID(r.Context(), pgUUIDFrom(nodeUUID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "registration_required", "node must register first")
		return uuid.Nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "lookup failed")
		return uuid.Nil, false
	}
	if node.Status == gen.NodeStatusRevoked {
		writeError(w, http.StatusForbidden, "node_revoked", "")
		return uuid.Nil, false
	}
	if node.FederationCertFingerprint != id.Fingerprint {
		writeError(w, http.StatusForbidden, "fingerprint_mismatch", "presented cert is not the active cert")
		return uuid.Nil, false
	}
	return nodeUUID, true
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
	node, ok := s.authNode(w, r)
	if !ok {
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
	changes := make([]wire.PinChange, len(rows))
	for i, row := range rows {
		changes[i] = wire.PinChange{
			Sequence: row.Sequence, AssignmentID: uuid.UUID(row.AssignmentID.Bytes).String(),
			Generation: row.Generation, Kind: row.Kind, CID: row.Cid, ByteSize: row.ByteSize,
		}
	}
	next := head
	if int64(len(rows)) == int64(limit) && len(rows) > 0 {
		next = rows[len(rows)-1].Sequence // full page: more may exist
	}
	slog.Info("fed.changes.served", "node_id", node, "since_seq", since, "returned", len(rows), "next_seq", next)
	writeJSON(w, http.StatusOK, wire.ChangesResponse{Changes: changes, NextSeq: next, CurrentEpoch: head})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	node, ok := s.authNode(w, r)
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
	items := make([]wire.SnapshotItem, len(rows))
	for i, row := range rows {
		items[i] = wire.SnapshotItem{
			CID: row.Cid, AssignmentID: uuid.UUID(row.AssignmentID.Bytes).String(),
			Generation: row.Generation, ByteSize: row.ByteSize, AssignedAt: row.AssignedAt.Format(time.RFC3339),
		}
	}
	nextCursor := ""
	if int64(len(rows)) == int64(limit) && len(rows) > 0 {
		nextCursor = rows[len(rows)-1].Cid
	}
	slog.Info("fed.snapshot.page", "node_id", node, "epoch", epoch, "returned", len(rows))
	writeJSON(w, http.StatusOK, wire.SnapshotResponse{Data: items, Cursor: nextCursor, SnapshotEpoch: epoch})
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	node, ok := s.authNode(w, r)
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
	node, ok := s.authNode(w, r)
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
