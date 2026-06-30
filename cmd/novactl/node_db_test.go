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

func TestNovactlSetDomainSetsVerifiedAt(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	id := uuid.New()
	seedNode(t, ctx, q, id, "sha256:setdomain")

	if err := setNodeDomain(ctx, q, gen.SetNodeDomainParams{
		ID: pgtype.UUID{Bytes: id, Valid: true}, Provider: "aws", FailureDomain: "us-east-1a",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := q.GetNodeByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		t.Fatal(err)
	}
	if !got.OperatorVerifiedAt.Valid {
		t.Fatal("set-domain must set operator_verified_at so the dimensions are trusted")
	}
	if got.Provider.String != "aws" || got.FailureDomainID.String != "us-east-1a" {
		t.Fatalf("dimensions = provider %q / fd %q", got.Provider.String, got.FailureDomainID.String)
	}
}

func TestNovactlSetDomainUnknownNode(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	err := setNodeDomain(ctx, q, gen.SetNodeDomainParams{ID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Provider: "x"})
	if err == nil {
		t.Fatal("expected error for unknown node")
	}
}
