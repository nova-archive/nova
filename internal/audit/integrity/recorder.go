package integrity

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
)

// Result values mirror the audit_result SQL enum (docs/specs/DATA_MODEL.sql).
const (
	ResultPass = "pass"
	ResultFail = "fail"
	ResultSkip = "skip"
)

// Finding is one check's outcome for one sampled item. Detail carries the
// failure reason (or skip rationale); it MUST NOT contain secret material
// (no key bytes, plaintext, or envelope bytes).
type Finding struct {
	CID    string
	Result string
	Detail string
}

// Recorder persists findings into integrity_audits and surfaces failures.
type Recorder struct {
	q    *gen.Queries
	sink FailureSink
	log  *slog.Logger
}

// NewRecorder returns a Recorder. sink must be non-nil (use NewNoopSink for the
// default); log defaults to slog.Default() when nil.
func NewRecorder(q *gen.Queries, sink FailureSink, log *slog.Logger) *Recorder {
	if sink == nil {
		sink = NewNoopSink()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{q: q, sink: sink, log: log}
}

// Record batch-inserts one integrity_audits row per finding, then (for each
// failure) emits a structured warn log and a FailureSink notification. An empty
// batch is a no-op.
func (r *Recorder) Record(ctx context.Context, kind Kind, findings []Finding) error {
	if len(findings) == 0 {
		return nil
	}
	params := make([]gen.InsertIntegrityAuditParams, 0, len(findings))
	for _, f := range findings {
		params = append(params, gen.InsertIntegrityAuditParams{
			Cid:       f.CID,
			AuditKind: gen.AuditKind(kind),
			Result:    gen.AuditResult(f.Result),
			Error:     pgtype.Text{String: f.Detail, Valid: f.Detail != ""},
		})
	}
	br := r.q.InsertIntegrityAudit(ctx, params)
	var insErr error
	br.Exec(func(_ int, err error) {
		if err != nil && insErr == nil {
			insErr = err
		}
	})
	if cerr := br.Close(); cerr != nil && insErr == nil {
		insErr = cerr
	}
	if insErr != nil {
		return fmt.Errorf("integrity: record %s: %w", kind, insErr)
	}

	for _, f := range findings {
		if f.Result != ResultFail {
			continue
		}
		r.log.WarnContext(ctx, "integrity audit failed",
			"audit_kind", string(kind), "cid", f.CID, "error", f.Detail)
		r.sink.AuditFailed(ctx, AuditFailure{CID: f.CID, Kind: kind, Detail: f.Detail})
	}
	return nil
}
