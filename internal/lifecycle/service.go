package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/db/gen"
)

// Service owns the owner/operator content lifecycle: soft-delete (reversible
// during the grace window) and the sweep that tombstones overdue soft-deletes
// via the shared TombstoneTree primitive. Each mutation runs in one pgx.Tx and
// writes its audit_log row inside that tx (the moderation precedent).
type Service struct {
	q       *gen.Queries
	pool    *pgxpool.Pool
	backend Backend
	cascade CascadeHook
	audit   *auditlog.Writer
	log     *slog.Logger
	now     func() time.Time
	grace   time.Duration
}

// NewService builds a Service. A nil cascade becomes a no-op; a nil clock
// becomes time.Now; a nil logger becomes slog.Default; a non-positive grace
// defaults to 24h.
func NewService(q *gen.Queries, pool *pgxpool.Pool, b Backend, c CascadeHook, a *auditlog.Writer, log *slog.Logger, now func() time.Time, grace time.Duration) *Service {
	if c == nil {
		c = func(context.Context, pgx.Tx, string, string) error { return nil }
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	if grace <= 0 {
		grace = 24 * time.Hour
	}
	return &Service{q: q, pool: pool, backend: b, cascade: c, audit: a, log: log, now: now, grace: grace}
}

// Grace returns the configured soft-delete grace window.
func (s *Service) Grace() time.Duration { return s.grace }

// SoftDelete flips a blob active → soft_deleted (reversible during the grace
// window), cascades that state to derivatives, and writes a blob.soft_deleted
// audit row — all in one tx. Returns ErrNotActive when the blob is absent or not
// active (the handler distinguishes 404 vs 409 via GetBlobMeta).
func (s *Service) SoftDelete(ctx context.Context, cidStr string, actor *uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	n, err := q.MarkSoftDeleted(ctx, cidStr)
	if err != nil {
		return fmt.Errorf("lifecycle: mark soft-deleted: %w", err)
	}
	if n == 0 {
		return ErrNotActive
	}
	if err := s.cascade(ctx, tx, cidStr, string(gen.BlobStateSoftDeleted)); err != nil {
		return fmt.Errorf("lifecycle: cascade: %w", err)
	}
	if s.audit != nil {
		if err := s.audit.WriteTx(ctx, tx, auditlog.Entry{
			ActorID: actor, Action: "blob.soft_deleted", TargetType: "cid", TargetID: cidStr,
			Payload: map[string]any{"cid": cidStr},
		}); err != nil {
			return fmt.Errorf("lifecycle: audit: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// tombstoneOverdue runs the irreversible tombstone for one overdue soft-delete:
// it collects derivatives (for the post-commit unpin), runs TombstoneTree + a
// system blob.tombstoned audit in one tx, commits, then best-effort unpins the
// parent + derivatives. A legal-held tree (which the sweep claim already filters)
// would surface as ErrLegalHold here; the caller logs and skips it.
func (s *Service) tombstoneOverdue(ctx context.Context, cidStr string) error {
	derivs, err := s.q.ListDerivativeCIDs(ctx, pgtype.Text{String: cidStr, Valid: true})
	if err != nil {
		return fmt.Errorf("lifecycle: list derivatives: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := TombstoneTree(ctx, q, tx, s.cascade, cidStr); err != nil {
		return err
	}
	if s.audit != nil {
		if err := s.audit.WriteTx(ctx, tx, auditlog.Entry{
			ActorID: nil, Action: "blob.tombstoned", TargetType: "cid", TargetID: cidStr,
			Payload: map[string]any{"cid": cidStr, "grace_seconds": int64(s.grace.Seconds())},
		}); err != nil {
			return fmt.Errorf("lifecycle: audit: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// Best-effort, idempotent, after commit — the bytes are already inert.
	s.unpin(ctx, cidStr)
	for _, d := range derivs {
		s.unpin(ctx, d)
	}
	return nil
}

// unpin best-effort removes the local Kubo pin (Phase-1 single-node; the
// federation unpin broadcast is Phase 2). Mirrors moderation.Service.unpin.
func (s *Service) unpin(ctx context.Context, cidStr string) {
	if s.backend == nil {
		return
	}
	c, err := cid.Decode(cidStr)
	if err != nil {
		s.log.Warn("lifecycle: bad cid for unpin", "cid", cidStr, "err", err)
		return
	}
	if err := s.backend.Unpin(ctx, c); err != nil {
		s.log.Warn("lifecycle: unpin", "cid", cidStr, "err", err)
	}
}
