package main

// P2-M7 (D-M7-6): voluntary graceful drain — the safe VOLUNTARY decommission
// primitive. Node-scoped, operator-initiated, one-shot, non-hysteretic; it is
// NOT the P2-M7.1 below-floor replacement queue. revoke stays the involuntary
// path. Steps 3–5 of the design (mark, fail pendings, enqueue) run in ONE
// transaction — the same bulk-transition contract as the liveness sweeper.

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
)

type drainResult struct {
	AlreadyDraining bool
	PendingCIDs     int64
	InflightCIDs    int64
}

// drainNode is the testable core.
func drainNode(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID, force bool) (drainResult, error) {
	var res drainResult
	q := gen.New(pool)
	st, err := q.GetNodeDrainState(ctx, id)
	if err != nil {
		return res, fmt.Errorf("node not found: %w", err)
	}
	if st.Status != gen.NodeStatusActive && st.Status != gen.NodeStatusSuspect {
		return res, fmt.Errorf("node is %s, not active/suspect — drain is for live nodes; use revoke for %s nodes", st.Status, st.Status)
	}
	if st.AssignmentSyncState != "current" && !force {
		return res, fmt.Errorf("node sync state is %q (unstable CID set); re-run with --force to drain anyway", st.AssignmentSyncState)
	}
	res.AlreadyDraining = st.DrainingAt.Valid

	tx, err := pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	qtx := gen.New(tx)
	if !res.AlreadyDraining {
		if _, err := qtx.SetNodeDraining(ctx, id); err != nil {
			return res, err
		}
	}
	if _, err := qtx.FailNodePendingAssignments(ctx, id); err != nil {
		return res, err
	}
	if err := qtx.MarkReplicationDirtyForNode(ctx, id); err != nil {
		return res, err
	}
	if err := qtx.EnqueueReconcileForNode(ctx, gen.EnqueueReconcileForNodeParams{
		Reason: "node_draining", NodeID: id,
	}); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}

	// The command just changed lifecycle state; if it cannot report debt, the
	// operator must know — never swallow these errors.
	pending, err := q.CountDrainPendingCIDs(ctx, id)
	if err != nil {
		return res, fmt.Errorf("count drain debt: %w", err)
	}
	inflight, err := q.CountDrainInflightCIDs(ctx, id)
	if err != nil {
		return res, fmt.Errorf("count drain inflight: %w", err)
	}
	res.PendingCIDs = pending
	res.InflightCIDs = inflight
	return res, nil
}

// undrainNode is the tiny explicit inverse (D-M7-6e).
func undrainNode(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := gen.New(tx)
	n, err := qtx.ClearNodeDraining(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("node not found or not draining")
	}
	if err := qtx.MarkReplicationDirtyForNode(ctx, id); err != nil {
		return err
	}
	if err := qtx.EnqueueReconcileForNode(ctx, gen.EnqueueReconcileForNodeParams{
		Reason: "node_undrained", NodeID: id,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func cmdNodeDrain(args []string) error {
	fs := flag.NewFlagSet("node drain", flag.ContinueOnError)
	idStr := fs.String("id", "", "node id (uuid)")
	force := fs.Bool("force", false, "drain even if assignment_sync_state != current")
	noConfirm := fs.Bool("no-confirm", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pgID, err := parsePGUUID(*idStr)
	if err != nil {
		return err
	}
	if !*noConfirm {
		fmt.Printf("Drain node %s? It stops receiving placements and stops counting toward durability, but keeps serving as a source while its replicas are rebuilt. [y/N]: ", *idStr)
		var ans string
		fmt.Scanln(&ans)
		if ans != "y" && ans != "Y" {
			return errors.New("aborted")
		}
	}
	return withNodeDBPool(func(ctx context.Context, pool *pgxpool.Pool) error {
		res, err := drainNode(ctx, pool, pgID, *force)
		if err != nil {
			return err
		}
		if res.AlreadyDraining {
			fmt.Printf("node %s was already draining — CIDs re-enqueued\n", *idStr)
		} else {
			fmt.Printf("draining node %s\n", *idStr)
		}
		fmt.Printf("drain debt: %d CIDs below target (%d with replacement in flight)\n", res.PendingCIDs, res.InflightCIDs)
		fmt.Println("next: keep the donor RUNNING; watch nova_node_drain_pending_cids; `novactl node revoke` only once debt is 0")
		return nil
	})
}

func cmdNodeUndrain(args []string) error {
	fs := flag.NewFlagSet("node undrain", flag.ContinueOnError)
	idStr := fs.String("id", "", "node id (uuid)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pgID, err := parsePGUUID(*idStr)
	if err != nil {
		return err
	}
	return withNodeDBPool(func(ctx context.Context, pool *pgxpool.Pool) error {
		if err := undrainNode(ctx, pool, pgID); err != nil {
			return err
		}
		fmt.Printf("node %s is no longer draining; countability/placement return on the next recompute\n", *idStr)
		return nil
	})
}
