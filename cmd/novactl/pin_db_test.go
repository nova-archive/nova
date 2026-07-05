package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/coordinator"
)

func TestPinAssignListUnpin(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	// seed node + blob + manifest (GetBlobSize reads blob_manifests.envelope_size,
	// so AssignPin requires a manifest row for the CID)
	node := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO nodes (id,nebula_cert_fingerprint,federation_cert_fingerprint,capacity_bytes,bandwidth_budget_bytes_per_day) VALUES ($1,$2,$3,0,0)`,
		pgtype.UUID{Bytes: node, Valid: true}, "neb", "fed"); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blobs (cid,mime_type,byte_size) VALUES ('bafy1','application/octet-stream',5)`); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blob_manifests (cid,hash_alg,codec,chunker,plaintext_size,envelope_size,block_count)
		VALUES ('bafy1','sha2-256','raw','size-262144',5,5,1)`); err != nil {
		t.Fatalf("seed blob_manifest: %v", err)
	}

	q := gen.New(pool)
	// assign via the same internal path the CLI uses
	tx, _ := pool.Begin(ctx)
	if _, err := coordinator.AssignPin(ctx, tx, "bafy1", node); err != nil {
		t.Fatal(err)
	}
	tx.Commit(ctx)

	desired, _ := q.ListDesiredAssignmentsByCID(ctx, "bafy1")
	verified, _ := q.ListVerifiedHoldersByCID(ctx, "bafy1")
	if len(desired) != 1 || len(verified) != 0 {
		t.Fatalf("desired=%d verified=%d (verified must be 0 in M3)", len(desired), len(verified))
	}

	// unpin removes the desired assignment
	tx, _ = pool.Begin(ctx)
	coordinator.UnpinPin(ctx, tx, "bafy1", node)
	tx.Commit(ctx)
	desired, _ = q.ListDesiredAssignmentsByCID(ctx, "bafy1")
	if len(desired) != 0 {
		t.Fatalf("after unpin desired = %d, want 0", len(desired))
	}
}
