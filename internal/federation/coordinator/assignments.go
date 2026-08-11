package coordinator

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

// Repair-source reservation errors (D-M5-8a). The caller re-picks a source.
var (
	ErrSourceIsDest        = errors.New("coordinator: repair source equals destination")
	ErrSourceNotSourceable = errors.New("coordinator: repair source is not repair-sourceable")
)

// ErrDestCannotFetch is returned when an assignment would target a donor that
// does not advertise blob-transfer/v1 (P2-M7.3, D-M7.3-22). The donor keeps
// everything it already holds; it simply cannot be given work it has no way to
// perform.
var ErrDestCannotFetch = errors.New("coordinator: assignment destination does not advertise " + wire.CapBlobTransfer)

// AllowAssignmentToDonorsWithoutBlobTransfer is a TEST-ONLY override for the
// two DB-direct assignment seams.
//
// It exists because those seams predate route-gating and some fixtures assign
// to synthetic nodes that advertise nothing. It is deliberately long, exported
// and unreferenced by any non-test code, so `grep` finds every use and a
// production caller reaching for it is obvious in review. Never set it outside
// a test.
var AllowAssignmentToDonorsWithoutBlobTransfer = false

// assertDestCanFetch is the guard at the last assignment path. The candidate
// queries filter the two SELECT-driven paths; this covers the seams that skip
// them (novactl `pin assign`, the admission assigner, the M5 scheduler).
func assertDestCanFetch(ctx context.Context, q *gen.Queries, dest uuid.UUID) error {
	if AllowAssignmentToDonorsWithoutBlobTransfer {
		return nil
	}
	ok, err := q.NodeAdvertisesCapability(ctx, gen.NodeAdvertisesCapabilityParams{
		ID: pgUUIDFrom(dest), Capability: wire.CapBlobTransfer,
	})
	if err != nil {
		return err
	}
	if !ok {
		return ErrDestCannotFetch
	}
	return nil
}

// AssignPinWithSource is the M5 scheduler's reservation primitive (D-M5-8a): it
// makes (cid,dest) a pending desired assignment bound to a durable repair source
// and appends a source-bearing assign change. source == uuid.Nil stores SQL NULL
// (coordinator-as-source — NEVER the synthetic CoordinatorSourceID, which lives
// only on the wire). A non-Nil source must differ from dest (ErrSourceIsDest) and
// be repair-sourceable + acked for this CID at reservation time
// (ErrSourceNotSourceable). The caller owns the tx and recomputes the projection
// (the orchestrator scheduler does so in the same tx) — keeping the projection
// dependency out of this package avoids an orchestrator↔coordinator import cycle.
func AssignPinWithSource(ctx context.Context, tx pgx.Tx, cid string, dest, source uuid.UUID) (Assignment, error) {
	if source != uuid.Nil && source == dest {
		return Assignment{}, ErrSourceIsDest
	}
	q := gen.New(tx)
	if err := assertDestCanFetch(ctx, q, dest); err != nil {
		return Assignment{}, err
	}
	if err := q.AcquireChangeLogLock(ctx); err != nil {
		return Assignment{}, err
	}
	size, err := q.GetBlobSize(ctx, cid)
	if err != nil {
		return Assignment{}, err // unknown blob ⇒ rollback, no orphan
	}
	var srcParam pgtype.UUID // zero value ⇒ SQL NULL (coordinator-as-source)
	if source != uuid.Nil {
		ok, err := q.IsRepairSourceableForCID(ctx, gen.IsRepairSourceableForCIDParams{ID: pgUUIDFrom(source), Cid: cid})
		if err != nil {
			return Assignment{}, err
		}
		if !ok {
			return Assignment{}, ErrSourceNotSourceable
		}
		srcParam = pgUUIDFrom(source)
	}
	up, err := q.UpsertPinAssignmentAssignWithSource(ctx, gen.UpsertPinAssignmentAssignWithSourceParams{
		Cid: cid, NodeID: pgUUIDFrom(dest), SourceNodeID: srcParam,
	})
	if err != nil {
		return Assignment{}, err
	}
	seq, err := q.InsertPinChangeWithSource(ctx, gen.InsertPinChangeWithSourceParams{
		NodeID: pgUUIDFrom(dest), AssignmentID: up.AssignmentID, Generation: up.Generation,
		Kind: wire.ChangeKindAssign, Cid: cid, ByteSize: size, SourceNodeID: srcParam,
	})
	if err != nil {
		return Assignment{}, err
	}
	slog.Info("fed.assign.source.txn", "cid", cid, "dest", dest, "source", source, "generation", up.Generation, "seq", seq)
	return Assignment{AssignmentID: up.AssignmentID.Bytes, Generation: up.Generation, Sequence: seq}, nil
}

// AssignPin makes (cid,node) a desired assignment: it upserts pin_assignments
// (new assignment_id + generation 1 on first assign; generation++ on re-assign,
// keeping the immutable assignment_id) and appends an 'assign' change. The
// advisory lock serializes change-log appends so sequences commit in assignment
// order (commit-order-safe). Caller owns the tx (novactl/tests in M3; the M5
// scheduler later).
func AssignPin(ctx context.Context, tx pgx.Tx, cid string, nodeID uuid.UUID) (Assignment, error) {
	q := gen.New(tx)
	if err := assertDestCanFetch(ctx, q, nodeID); err != nil {
		return Assignment{}, err
	}
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
