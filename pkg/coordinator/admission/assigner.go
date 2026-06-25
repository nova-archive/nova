// Package admission implements the minimal admission assigner: on successful
// blob commit it selects up to R(class) source-capable donor nodes by
// reputation/liveness and creates pin assignments via the coordinator.AssignPin
// seam. Anti-affinity, healing, and the async commit gate are M5/Task 11.
package admission

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/db/gen"
	coordinator "github.com/nova-archive/nova/internal/federation/coordinator"
)

// Assigner selects up to R(class) sourceable donor nodes and creates pin
// assignments in a single transaction. It satisfies the storage.Assigner
// interface so it can be injected without creating an import cycle.
type Assigner struct {
	pool   *pgxpool.Pool
	factor config.ReplicationFactor
}

// New returns an Assigner backed by the given pool and replication factor.
func New(pool *pgxpool.Pool, factor config.ReplicationFactor) *Assigner {
	return &Assigner{pool: pool, factor: factor}
}

// Assign selects up to rFor(class) source-capable donors by reputation/liveness
// and calls coordinator.AssignPin for each in one transaction.
//
// If the blob has no manifest (cid unknown), it returns (0, err).
// If fewer than R(class) eligible donors exist, it assigns what exists, logs
// admission.under_replicated, and returns (got, nil) — not an error.
// The caller (Put gate-off path) treats any returned error as best-effort-only.
func (a *Assigner) Assign(ctx context.Context, cid, class string) (assigned int, err error) {
	r := rFor(class, a.factor)
	q := gen.New(a.pool)

	// GetBlobSize confirms the blob has a committed manifest.
	size, err := q.GetBlobSize(ctx, cid)
	if err != nil {
		return 0, fmt.Errorf("admission: GetBlobSize(%s): %w", cid, err)
	}

	// MinFreeBytes uses pgtype.Int8 (nullable bigint).
	cands, err := q.ListAdmissionCandidates(ctx, gen.ListAdmissionCandidatesParams{
		MinFreeBytes: pgtype.Int8{Int64: size, Valid: true},
		Lim:          int32(r),
	})
	if err != nil {
		return 0, fmt.Errorf("admission: ListAdmissionCandidates: %w", err)
	}

	if len(cands) == 0 {
		if r > 0 {
			slog.Warn("admission.under_replicated", "cid", cid, "want", r, "got", 0)
		}
		return 0, nil
	}

	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("admission: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, cand := range cands {
		nodeID := uuid.UUID(cand.NodeID.Bytes)
		if _, aerr := coordinator.AssignPin(ctx, tx, cid, nodeID); aerr != nil {
			return 0, fmt.Errorf("admission: AssignPin(cid=%s node=%s): %w", cid, nodeID, aerr)
		}
		assigned++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("admission: commit: %w", err)
	}

	if assigned < r {
		slog.Warn("admission.under_replicated", "cid", cid, "want", r, "got", assigned)
	}
	return assigned, nil
}

// rFor maps a content class string to the configured replication factor.
// Unknown classes default to Important (the safe, high-durability default).
func rFor(class string, f config.ReplicationFactor) int {
	switch class {
	case "important":
		return f.Important
	case "normal":
		return f.Normal
	case "cache":
		return f.Cache
	default:
		return f.Important
	}
}
