package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
)

func TestNodeTrustClearReviewResetsMarkerAndEpoch(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	id := uuid.New()
	pg := pgtype.UUID{Bytes: id, Valid: true}
	seedNode(t, ctx, q, id, "sha256:trust")

	if err := q.SetTrustReview(ctx, gen.SetTrustReviewParams{
		ID:                pg,
		TrustReviewReason: pgtype.Text{String: "hash_mismatch", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	before, err := q.GetNodeTrust(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	if !before.TrustReviewRequiredAt.Valid {
		t.Fatal("expected trust_review_required_at to be set before clear")
	}
	epochBefore := before.TrustEpochStartedAt

	time.Sleep(time.Millisecond)

	if err := clearNodeTrustReview(ctx, q, pg); err != nil {
		t.Fatal(err)
	}

	after, err := q.GetNodeTrust(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	if after.TrustReviewRequiredAt.Valid {
		t.Fatal("trust_review_required_at must be NULL after clear-review")
	}
	if after.TrustEpochStartedAt.Before(epochBefore) {
		t.Fatalf("epoch regressed: before=%v after=%v", epochBefore, after.TrustEpochStartedAt)
	}
}

func TestNodeTrustSuspendUnsuspend(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	id := uuid.New()
	pg := pgtype.UUID{Bytes: id, Valid: true}
	seedNode(t, ctx, q, id, "sha256:suspend")

	if err := setNodeTrustState(ctx, q, pg, "suspended"); err != nil {
		t.Fatal(err)
	}
	got, err := q.GetNodeTrust(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrustState != "suspended" {
		t.Fatalf("trust_state = %q, want %q", got.TrustState, "suspended")
	}

	if err := setNodeTrustState(ctx, q, pg, "probationary"); err != nil {
		t.Fatal(err)
	}
	got, err = q.GetNodeTrust(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrustState != "probationary" {
		t.Fatalf("trust_state = %q, want %q", got.TrustState, "probationary")
	}
}
