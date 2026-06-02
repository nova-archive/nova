package integrity

import "context"

// AuditFailure describes a single failed finding handed to a FailureSink.
// It never carries secret material — only the CID, the kind, and an error
// detail string.
type AuditFailure struct {
	CID    string
	Kind   Kind
	Detail string
}

// FailureSink is notified of each audit failure. It is the deferral seam for
// the integrity.audit_failed outbound webhook (and any future failure metric):
// a delivery implementation drops in here without touching the audit core.
//
// The Recorder always emits a structured warn log per failure independently of
// the sink, so failures are surfaced even with the default no-op sink.
type FailureSink interface {
	AuditFailed(ctx context.Context, f AuditFailure)
}

type noopSink struct{}

// NewNoopSink returns the default FailureSink, which performs no external
// delivery. With it, failure surfacing is the Recorder's warn logs plus the
// integrity_audits rows plus the admin listing endpoint.
func NewNoopSink() FailureSink { return noopSink{} }

func (noopSink) AuditFailed(context.Context, AuditFailure) {}
