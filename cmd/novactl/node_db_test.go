package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
)

func seedNode(t *testing.T, ctx context.Context, q *gen.Queries, id uuid.UUID, fp string) {
	t.Helper()
	_, err := q.RegisterNode(ctx, gen.RegisterNodeParams{
		ID:                        pgtype.UUID{Bytes: id, Valid: true},
		NebulaCertFingerprint:     "sha256:n",
		FederationCertFingerprint: fp,
		PolicyFilters:             []byte("{}"),
		AdvertisedCapabilities:    []string{},
		RequiredCapabilities:      []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRevokeNodeCore(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	id := uuid.New()
	seedNode(t, ctx, q, id, "sha256:f")
	if err := revokeNode(ctx, q, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := q.GetNodeByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if got.Status != gen.NodeStatusRevoked {
		t.Fatalf("status = %v", got.Status)
	}
}

func TestRotateNodeCertCore(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	id := uuid.New()
	seedNode(t, ctx, q, id, "sha256:old")
	if err := rotateNodeCert(ctx, q, pgtype.UUID{Bytes: id, Valid: true}, "sha256:new"); err != nil {
		t.Fatal(err)
	}
	got, _ := q.GetNodeByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if got.FederationCertFingerprint != "sha256:new" || !got.CertRotatedAt.Valid {
		t.Fatalf("rotate not applied: fp=%s rotated=%v", got.FederationCertFingerprint, got.CertRotatedAt.Valid)
	}
}
