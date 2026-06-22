package coordinator

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// Assignment is the result of a mutation: the immutable handle, its current
// generation, and the change-log sequence emitted.
type Assignment struct {
	AssignmentID uuid.UUID
	Generation   int64
	Sequence     int64
}

// AssignPin makes (cid,node) a desired assignment: it upserts pin_assignments
// (new assignment_id + generation 1 on first assign; generation++ on re-assign,
// keeping the immutable assignment_id) and appends an 'assign' change. The
// advisory lock serializes change-log appends so sequences commit in assignment
// order (commit-order-safe). Caller owns the tx (novactl/tests in M3; the M5
// scheduler later).
func AssignPin(ctx context.Context, tx pgx.Tx, cid string, nodeID uuid.UUID) (Assignment, error) {
	q := gen.New(tx)
	if err := q.AcquireChangeLogLock(ctx); err != nil {
		return Assignment{}, err
	}
	size, err := q.GetBlobSize(ctx, cid)
	if err != nil {
		return Assignment{}, err // unknown blob ⇒ rollback, no orphan
	}
	up, err := q.UpsertPinAssignmentAssign(ctx, gen.UpsertPinAssignmentAssignParams{
		Cid: cid, NodeID: pgUUIDFrom(nodeID),
	})
	if err != nil {
		return Assignment{}, err
	}
	seq, err := q.InsertPinChange(ctx, gen.InsertPinChangeParams{
		NodeID: pgUUIDFrom(nodeID), AssignmentID: up.AssignmentID,
		Generation: up.Generation, Kind: wire.ChangeKindAssign, Cid: cid, ByteSize: size,
	})
	if err != nil {
		return Assignment{}, err
	}
	slog.Info("fed.assign.txn", "cid", cid, "node_id", nodeID, "generation", up.Generation, "seq", seq)
	return Assignment{AssignmentID: up.AssignmentID.Bytes, Generation: up.Generation, Sequence: seq}, nil
}

// UnpinPin retires a desired assignment: it appends an 'unpin' change at the next
// generation and deletes the live row. The history survives in the change log.
func UnpinPin(ctx context.Context, tx pgx.Tx, cid string, nodeID uuid.UUID) (Assignment, error) {
	q := gen.New(tx)
	if err := q.AcquireChangeLogLock(ctx); err != nil {
		return Assignment{}, err
	}
	cur, err := q.GetPinAssignmentForUpdate(ctx, gen.GetPinAssignmentForUpdateParams{Cid: cid, NodeID: pgUUIDFrom(nodeID)})
	if err != nil {
		return Assignment{}, err // pgx.ErrNoRows ⇒ not assigned
	}
	nextGen := cur.Generation + 1
	seq, err := q.InsertPinChange(ctx, gen.InsertPinChangeParams{
		NodeID: pgUUIDFrom(nodeID), AssignmentID: cur.AssignmentID,
		Generation: nextGen, Kind: wire.ChangeKindUnpin, Cid: cid, ByteSize: 0,
	})
	if err != nil {
		return Assignment{}, err
	}
	if _, err := q.DeletePinAssignment(ctx, gen.DeletePinAssignmentParams{Cid: cid, NodeID: pgUUIDFrom(nodeID)}); err != nil {
		return Assignment{}, err
	}
	slog.Info("fed.unpin.txn", "cid", cid, "node_id", nodeID, "generation", nextGen, "seq", seq)
	return Assignment{AssignmentID: cur.AssignmentID.Bytes, Generation: nextGen, Sequence: seq}, nil
}
