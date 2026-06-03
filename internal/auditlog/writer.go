// Package auditlog writes the durable operator-action trail (audit_log). It has
// two modes: WriteTx (atomic, inside a moderation tx) and Write (best-effort,
// for post-commit backfilled call sites that must never be failed by audit I/O).
package auditlog

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
)

// Entry describes a single operator action written to the audit_log table.
type Entry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Payload    map[string]any
}

// Writer holds the query handle and logger used by both write modes.
type Writer struct {
	q   *gen.Queries
	log *slog.Logger
}

// NewWriter constructs a Writer backed by q (typically gen.New(pool)).
func NewWriter(q *gen.Queries, log *slog.Logger) *Writer { return &Writer{q: q, log: log} }

func (w *Writer) params(e Entry) (gen.InsertAuditLogParams, error) {
	payload := []byte("{}")
	if e.Payload != nil {
		b, err := json.Marshal(e.Payload)
		if err != nil {
			return gen.InsertAuditLogParams{}, err
		}
		payload = b
	}
	actor := pgtype.UUID{}
	if e.ActorID != nil {
		actor = pgtype.UUID{Bytes: [16]byte(*e.ActorID), Valid: true}
	}
	return gen.InsertAuditLogParams{
		ActorID:    actor,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Payload:    payload,
	}, nil
}

// WriteTx inserts one audit_log row atomically inside tx.  An error from the
// database causes the caller's surrounding transaction to be rolled back,
// which is correct for legally-sensitive moderation actions.
func (w *Writer) WriteTx(ctx context.Context, tx pgx.Tx, e Entry) error {
	p, err := w.params(e)
	if err != nil {
		return err
	}
	return w.q.WithTx(tx).InsertAuditLog(ctx, p)
}

// Write inserts best-effort on the pool.  Failures are logged via slog.Warn
// and never returned to the caller; the calling code path has already
// committed its own transaction and must not be failed by audit I/O.
func (w *Writer) Write(ctx context.Context, e Entry) {
	p, err := w.params(e)
	if err != nil {
		w.log.Warn("auditlog: marshal", "action", e.Action, "err", err)
		return
	}
	if err := w.q.InsertAuditLog(ctx, p); err != nil {
		w.log.Warn("auditlog: write", "action", e.Action, "err", err)
	}
}
