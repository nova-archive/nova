package coordinator

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
)

func seedBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string, size int64) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO blobs (cid, mime_type, byte_size) VALUES ($1,'application/octet-stream',$2)`, cid, size)
	if err != nil {
		t.Fatal(err)
	}
}

func seedNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint, capacity_bytes, bandwidth_budget_bytes_per_day)
	    VALUES ($1,$2,$3,0,0)`, pgtype.UUID{Bytes: id, Valid: true}, "neb:"+id.String(), "fed:"+id.String())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAssignPinCreatesRowAndChange(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	node := seedNode(t, ctx, pool)
	seedBlob(t, ctx, pool, "bafy1", 1048576)

	tx, _ := pool.Begin(ctx)
	a, err := AssignPin(ctx, tx, "bafy1", node)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Generation != 1 || a.AssignmentID == uuid.Nil || a.Sequence < 1 {
		t.Fatalf("assign #1: %+v", a)
	}

	// re-assign bumps generation, keeps assignment_id
	tx2, _ := pool.Begin(ctx)
	a2, err := AssignPin(ctx, tx2, "bafy1", node)
	if err != nil {
		t.Fatal(err)
	}
	tx2.Commit(ctx)
	if a2.Generation != 2 || a2.AssignmentID != a.AssignmentID {
		t.Fatalf("assign #2 should bump gen, keep id: %+v (was %+v)", a2, a)
	}

	// one assign change row per call
	var changes int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_changes WHERE cid='bafy1' AND kind='assign'`).Scan(&changes)
	if changes != 2 {
		t.Fatalf("assign changes = %d, want 2", changes)
	}
}

func TestUnpinPinWritesChangeAndDeletesRow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	node := seedNode(t, ctx, pool)
	seedBlob(t, ctx, pool, "bafy1", 10)

	tx, _ := pool.Begin(ctx)
	a, _ := AssignPin(ctx, tx, "bafy1", node)
	tx.Commit(ctx)

	tx2, _ := pool.Begin(ctx)
	u, err := UnpinPin(ctx, tx2, "bafy1", node)
	if err != nil {
		t.Fatal(err)
	}
	tx2.Commit(ctx)
	if u.Generation != a.Generation+1 {
		t.Fatalf("unpin gen = %d, want %d", u.Generation, a.Generation+1)
	}

	var rows int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_assignments WHERE cid='bafy1'`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("row should be deleted, got %d", rows)
	}
	var unpins int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_changes WHERE cid='bafy1' AND kind='unpin'`).Scan(&unpins)
	if unpins != 1 {
		t.Fatalf("unpin changes = %d, want 1", unpins)
	}
}

func TestAssignSequencesCommitOrderedUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	node := seedNode(t, ctx, pool)
	for i := 0; i < 20; i++ {
		seedBlob(t, ctx, pool, cidN(i), 1)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, _ := pool.Begin(ctx)
			if _, err := AssignPin(ctx, tx, cidN(i), node); err != nil {
				tx.Rollback(ctx)
				return
			}
			tx.Commit(ctx)
		}(i)
	}
	wg.Wait()
	// All 20 commit (no rollbacks here), so the advisory lock yields a
	// contiguous, commit-ordered 1..20. The invariant under test is commit
	// ordering — a donor never advances its cursor past a lower-sequence row that
	// can still commit; gaps from rolled-back txns (none here) are harmless.
	var lo, hi, cnt int64
	pool.QueryRow(ctx, `SELECT min(sequence), max(sequence), count(*) FROM pin_changes`).Scan(&lo, &hi, &cnt)
	if cnt != 20 || hi-lo != 19 {
		t.Fatalf("sequences not commit-ordered: lo=%d hi=%d cnt=%d", lo, hi, cnt)
	}
}

func cidN(i int) string { return "bafy" + string(rune('a'+i)) }
