package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/coordinator"
)

func cmdPin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: novactl pin <assign|unpin|list> ...")
	}
	switch args[0] {
	case "assign":
		return cmdPinAssign(args[1:])
	case "unpin":
		return cmdPinUnpin(args[1:])
	case "list":
		return cmdPinList(args[1:])
	default:
		return fmt.Errorf("novactl pin: unknown subcommand %q", args[0])
	}
}

func pinMutate(args []string, name string, fn func(ctx context.Context, tx pgx.Tx, cid string, node uuid.UUID) (coordinator.Assignment, error)) error {
	fs := flag.NewFlagSet("pin "+name, flag.ContinueOnError)
	cid := fs.String("cid", "", "blob CID (required)")
	nodeStr := fs.String("node", "", "donor node UUID (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	node, err := uuid.Parse(*nodeStr)
	if err != nil {
		return fmt.Errorf("invalid --node: %w", err)
	}
	return withNodeDBPool(func(ctx context.Context, pool *pgxpool.Pool) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		a, err := fn(ctx, tx, *cid, node)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		fmt.Printf("%s cid=%s node=%s assignment_id=%s generation=%d sequence=%d\n", name, *cid, node, a.AssignmentID, a.Generation, a.Sequence)
		return nil
	})
}

func cmdPinAssign(args []string) error { return pinMutate(args, "assign", coordinator.AssignPin) }
func cmdPinUnpin(args []string) error  { return pinMutate(args, "unpin", coordinator.UnpinPin) }

func cmdPinList(args []string) error {
	fs := flag.NewFlagSet("pin list", flag.ContinueOnError)
	cid := fs.String("cid", "", "list by blob CID")
	nodeStr := fs.String("node", "", "list by donor node UUID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		switch {
		case *cid != "":
			desired, err := q.ListDesiredAssignmentsByCID(ctx, *cid)
			if err != nil {
				return err
			}
			verified, err := q.ListVerifiedHoldersByCID(ctx, *cid)
			if err != nil {
				return err
			}
			fmt.Printf("desired assignments (%d):\n", len(desired))
			for _, d := range desired {
				fmt.Printf("  node=%s generation=%d state=%s\n", uuid.UUID(d.NodeID.Bytes), d.Generation, d.State)
			}
			fmt.Printf("verified holders (%d):\n", len(verified))
			for _, v := range verified {
				fmt.Printf("  node=%s generation=%d\n", uuid.UUID(v.NodeID.Bytes), v.Generation)
			}
		case *nodeStr != "":
			node, err := uuid.Parse(*nodeStr)
			if err != nil {
				return err
			}
			rows, err := q.ListDesiredAssignmentsByNode(ctx, pgtype.UUID{Bytes: node, Valid: true})
			if err != nil {
				return err
			}
			fmt.Printf("desired assignments for node %s (%d):\n", node, len(rows))
			for _, r := range rows {
				fmt.Printf("  cid=%s generation=%d state=%s\n", r.Cid, r.Generation, r.State)
			}
		default:
			return fmt.Errorf("pin list: pass --cid or --node")
		}
		return nil
	})
}
