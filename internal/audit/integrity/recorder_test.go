package integrity_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/nova-archive/nova/internal/audit/integrity"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

type spySink struct{ failures []integrity.AuditFailure }

func (s *spySink) AuditFailed(_ context.Context, f integrity.AuditFailure) {
	s.failures = append(s.failures, f)
}

func TestRecorderInsertsRowsAndDispatchesFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping recorder DB test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	spy := &spySink{}
	rec := integrity.NewRecorder(q, spy, slog.Default())

	err := rec.Record(ctx, integrity.KindKuboPinPresent, []integrity.Finding{
		{CID: "bafyA", Result: integrity.ResultPass},
		{CID: "bafyB", Result: integrity.ResultFail, Detail: "not pinned"},
		{CID: "bafyC", Result: integrity.ResultSkip},
	})
	require.NoError(t, err)

	// One row per finding.
	n, err := q.CountIntegrityAudits(ctx, gen.CountIntegrityAuditsParams{})
	require.NoError(t, err)
	require.EqualValues(t, 3, n)

	// Only the failure reached the sink, with full context.
	require.Len(t, spy.failures, 1)
	require.Equal(t, "bafyB", spy.failures[0].CID)
	require.Equal(t, integrity.KindKuboPinPresent, spy.failures[0].Kind)
	require.Equal(t, "not pinned", spy.failures[0].Detail)

	// The failure is filterable in the listing.
	fails, err := q.CountIntegrityAudits(ctx, gen.CountIntegrityAuditsParams{
		Result: gen.NullAuditResult{AuditResult: gen.AuditResultFail, Valid: true},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, fails)

	// Empty batch is a no-op.
	require.NoError(t, rec.Record(ctx, integrity.KindKuboPinPresent, nil))
}
