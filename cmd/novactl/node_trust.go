package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
)

// clearNodeTrustReview is the testable core: it clears the hash-mismatch review
// marker and restarts the trust epoch (D-M6-8).
func clearNodeTrustReview(ctx context.Context, q *gen.Queries, pgID pgtype.UUID) error {
	return q.ClearTrustReview(ctx, pgID)
}

// setNodeTrustState is the testable core: it sets trust_state to state.
func setNodeTrustState(ctx context.Context, q *gen.Queries, pgID pgtype.UUID, state string) error {
	return q.SetTrustState(ctx, gen.SetTrustStateParams{ID: pgID, TrustState: state})
}

func cmdNodeTrust(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: novactl node trust <clear-review|suspend|unsuspend> <node_id>")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "clear-review":
		if len(rest) == 0 {
			return fmt.Errorf("usage: novactl node trust clear-review <node_id>")
		}
		pgID, err := parsePGUUID(rest[0])
		if err != nil {
			return err
		}
		return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
			if err := clearNodeTrustReview(ctx, q, pgID); err != nil {
				return err
			}
			fmt.Printf("cleared trust review for node %s (epoch restarted)\n", rest[0])
			return nil
		})
	case "suspend":
		if len(rest) == 0 {
			return fmt.Errorf("usage: novactl node trust suspend <node_id>")
		}
		pgID, err := parsePGUUID(rest[0])
		if err != nil {
			return err
		}
		return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
			if err := setNodeTrustState(ctx, q, pgID, "suspended"); err != nil {
				return err
			}
			fmt.Printf("suspended node %s\n", rest[0])
			return nil
		})
	case "unsuspend":
		if len(rest) == 0 {
			return fmt.Errorf("usage: novactl node trust unsuspend <node_id>")
		}
		pgID, err := parsePGUUID(rest[0])
		if err != nil {
			return err
		}
		return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
			if err := setNodeTrustState(ctx, q, pgID, "probationary"); err != nil {
				return err
			}
			fmt.Printf("unsuspended node %s (trust_state=probationary)\n", rest[0])
			return nil
		})
	default:
		return fmt.Errorf("usage: novactl node trust <clear-review|suspend|unsuspend> <node_id>")
	}
}
