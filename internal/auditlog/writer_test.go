package auditlog_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestWriteTxIsAtomicWithItsTx(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	w := auditlog.NewWriter(gen.New(pool), slog.Default())

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, w.WriteTx(ctx, tx, auditlog.Entry{
		Action:     "dmca.quarantined",
		TargetType: "cid",
		TargetID:   "bafyX",
		Payload:    map[string]any{"reason": "test"},
	}))
	require.NoError(t, tx.Rollback(ctx)) // rolled back ⇒ no row

	n, err := gen.New(pool).CountAuditLog(ctx, gen.CountAuditLogParams{})
	require.NoError(t, err)
	require.EqualValues(t, 0, n)
}

func TestWriteTxCommitPersistsRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	w := auditlog.NewWriter(gen.New(pool), slog.Default())

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, w.WriteTx(ctx, tx, auditlog.Entry{
		Action:     "dmca.quarantined",
		TargetType: "cid",
		TargetID:   "bafyY",
		Payload:    map[string]any{"reason": "commit-test"},
	}))
	require.NoError(t, tx.Commit(ctx)) // committed ⇒ row persists

	n, err := gen.New(pool).CountAuditLog(ctx, gen.CountAuditLogParams{})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

func TestWriteBestEffortInsertsRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	w := auditlog.NewWriter(gen.New(pool), slog.Default())

	w.Write(ctx, auditlog.Entry{
		Action:     "admin.backfill",
		TargetType: "cid",
		TargetID:   "bafyZ",
		Payload:    map[string]any{"source": "best-effort"},
	})

	n, err := gen.New(pool).CountAuditLog(ctx, gen.CountAuditLogParams{})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}
