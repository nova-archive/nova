package moderation_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/moderation"
)

// setSchedulePast forces a quarantine decision's scheduled_tombstone_at into the
// past so a single Tick treats it as overdue (Quarantine sets it to a future
// now()+TombstoneAfter).
func setSchedulePast(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`UPDATE moderation_decisions SET scheduled_tombstone_at = now() - interval '1 hour'
		 WHERE cid=$1 AND action='quarantine'`, cidStr)
	require.NoError(t, err)
}

func TestSweeperTombstonesOverdueOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	ks := newKeystore(t, ctx, pool)
	be := newEmbeddedBackend(t, ctx)
	svc := newService(pool, &fakeBackend{}, time.Now)

	// Overdue, non-legal-hold → should be tombstoned this tick.
	overdue := seedParentWithDerivative(t, ctx, pool, ks, be)
	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: overdue.parentCID, Rule: "dmca", Reason: "x", TombstoneAfter: time.Hour}))
	setSchedulePast(t, ctx, pool, overdue.parentCID)

	// Future schedule → untouched.
	future := seedParentWithDerivative(t, ctx, pool, ks, be)
	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: future.parentCID, Rule: "dmca", Reason: "x", TombstoneAfter: 24 * time.Hour}))

	// Legal-hold quarantine with a (malformed) past schedule → the listing's
	// legal_hold filter must skip it; it stays quarantined.
	held := seedParentWithDerivative(t, ctx, pool, ks, be)
	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: held.parentCID, Rule: "severe_content", Reason: "x", LegalHold: true}))
	setSchedulePast(t, ctx, pool, held.parentCID)

	moderation.NewSweeper(svc, time.Minute, true, nil).Tick(ctx)

	require.Equal(t, "tombstoned", blobState(t, ctx, pool, overdue.parentCID), "overdue quarantine is tombstoned")
	require.Equal(t, "quarantined", blobState(t, ctx, pool, future.parentCID), "future schedule is untouched")
	require.Equal(t, "quarantined", blobState(t, ctx, pool, held.parentCID), "legal-hold row is skipped by the sweep filter")
}

func TestSweeperDisabledReturnsImmediately(t *testing.T) {
	sw := moderation.NewSweeper(nil, time.Hour, false, nil)
	done := make(chan struct{})
	go func() { sw.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disabled sweeper Run did not return immediately")
	}
}
