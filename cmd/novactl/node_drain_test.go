package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
)

// P2-M7 (D-M7-6) drain/undrain core tests, mirroring TestRevokeNodeCore's
// harness. Fixture: node A active/current with one acked and one pending
// assignment.

func seedDrainableNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	q := gen.New(pool)
	id := uuid.New()
	seedNode(t, ctx, q, id, "sha256:drain")
	if _, err := pool.Exec(ctx,
		`UPDATE nodes SET assignment_sync_state = 'current' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	for _, b := range []struct{ cid, state string }{
		{"drain-b1", "acked"}, {"drain-b2", "pending"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
			VALUES ($1, 'image/jpeg', 1000, 'active', 'image', 2)`, b.cid); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO pin_assignments (cid, node_id, state) VALUES ($1, $2, $3)`,
			b.cid, id, b.state); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func queueReason(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string) string {
	t.Helper()
	var reason string
	if err := pool.QueryRow(ctx,
		`SELECT reason FROM blob_replication_reconcile_queue WHERE cid = $1`, cid).
		Scan(&reason); err != nil {
		t.Fatalf("queue row for %s: %v", cid, err)
	}
	return reason
}

func drainStateOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) gen.GetNodeDrainStateRow {
	t.Helper()
	st, err := gen.New(pool).GetNodeDrainState(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestDrainNodeCore(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	id := seedDrainableNode(t, ctx, pool)

	res, err := drainNode(ctx, pool, pgtype.UUID{Bytes: id, Valid: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.AlreadyDraining {
		t.Fatal("first drain must report AlreadyDraining == false")
	}
	if st := drainStateOf(t, ctx, pool, id); !st.DrainingAt.Valid {
		t.Fatal("draining_at must be set")
	}

	// The pending reservation was failed in the same transaction.
	var pendingState string
	if err := pool.QueryRow(ctx,
		`SELECT state FROM pin_assignments WHERE cid = 'drain-b2' AND node_id = $1`, id).
		Scan(&pendingState); err != nil {
		t.Fatal(err)
	}
	if pendingState != "failed" {
		t.Fatalf("pending assignment state = %q, want failed", pendingState)
	}

	// Every CID the node held is enqueued with reason node_draining.
	for _, cid := range []string{"drain-b1", "drain-b2"} {
		if r := queueReason(t, ctx, pool, cid); r != "node_draining" {
			t.Fatalf("queue reason for %s = %q, want node_draining", cid, r)
		}
	}
}

func TestDrainNodeIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	id := seedDrainableNode(t, ctx, pool)
	pgID := pgtype.UUID{Bytes: id, Valid: true}

	if _, err := drainNode(ctx, pool, pgID, false); err != nil {
		t.Fatal(err)
	}
	first := drainStateOf(t, ctx, pool, id).DrainingAt.Time

	res, err := drainNode(ctx, pool, pgID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyDraining {
		t.Fatal("second drain must report AlreadyDraining == true")
	}
	second := drainStateOf(t, ctx, pool, id).DrainingAt.Time
	if !second.Equal(first) {
		t.Fatalf("draining_at changed on re-drain: %v → %v", first, second)
	}
	// Re-enqueue without error: queue rows still present.
	if r := queueReason(t, ctx, pool, "drain-b1"); r != "node_draining" {
		t.Fatalf("queue reason = %q", r)
	}
}

func TestDrainNodeRefusals(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	id := seedDrainableNode(t, ctx, pool)
	pgID := pgtype.UUID{Bytes: id, Valid: true}

	if _, err := pool.Exec(ctx, `UPDATE nodes SET status = 'revoked' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := drainNode(ctx, pool, pgID, false); err == nil ||
		!strings.Contains(err.Error(), "not active/suspect") {
		t.Fatalf("revoked node: err = %v, want mention of not active/suspect", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE nodes SET status = 'active', assignment_sync_state = 'reconciling' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := drainNode(ctx, pool, pgID, false); err == nil ||
		!strings.Contains(err.Error(), "--force") {
		t.Fatalf("non-current sync: err = %v, want mention of --force", err)
	}

	if _, err := drainNode(ctx, pool, pgID, true); err != nil {
		t.Fatalf("force drain: %v", err)
	}
	if st := drainStateOf(t, ctx, pool, id); !st.DrainingAt.Valid {
		t.Fatal("force drain must set draining_at")
	}
}

func TestUndrainNodeCore(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	id := seedDrainableNode(t, ctx, pool)
	pgID := pgtype.UUID{Bytes: id, Valid: true}

	if err := undrainNode(ctx, pool, pgID); err == nil ||
		!strings.Contains(err.Error(), "not draining") {
		t.Fatalf("undrain of non-draining node: err = %v, want 'not draining'", err)
	}

	if _, err := drainNode(ctx, pool, pgID, false); err != nil {
		t.Fatal(err)
	}
	if err := undrainNode(ctx, pool, pgID); err != nil {
		t.Fatal(err)
	}
	if st := drainStateOf(t, ctx, pool, id); st.DrainingAt.Valid {
		t.Fatal("draining_at must be NULL after undrain")
	}
	for _, cid := range []string{"drain-b1", "drain-b2"} {
		if r := queueReason(t, ctx, pool, cid); r != "node_undrained" {
			t.Fatalf("queue reason for %s = %q, want node_undrained", cid, r)
		}
	}
}
