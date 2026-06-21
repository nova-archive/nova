package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func seedUser(t *testing.T, ctx context.Context, q *gen.Queries, role gen.UserRole, email string) uuid.UUID {
	t.Helper()
	row, err := q.CreateUser(ctx, gen.CreateUserParams{
		Email:        email,
		Role:         role,
		PasswordHash: pgtype.Text{String: "x", Valid: true},
	})
	require.NoError(t, err)
	return uuid.UUID(row.ID.Bytes)
}

func TestCreateCollectionCore(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	owner := seedUser(t, ctx, q, gen.UserRoleOperator, "op@example.test")

	got, err := createCollection(ctx, q, collectionParams{
		OwnerID: owner, Name: "Public", Slug: "public", Visibility: "public",
	})
	require.NoError(t, err)
	require.Equal(t, "public", string(got.Visibility))
	require.Equal(t, "public", got.Slug)
	require.Equal(t, owner, uuid.UUID(got.OwnerID.Bytes))
}

func TestResolveCollectionOwnerSoleOperator(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	op := seedUser(t, ctx, q, gen.UserRoleOperator, "op@example.test")
	seedUser(t, ctx, q, gen.UserRoleViewer, "v@example.test") // non-operator ignored

	got, err := resolveCollectionOwner(ctx, q, "")
	require.NoError(t, err)
	require.Equal(t, op, got)
}

func TestResolveCollectionOwnerAmbiguous(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	seedUser(t, ctx, q, gen.UserRoleOperator, "op1@example.test")
	seedUser(t, ctx, q, gen.UserRoleOperator, "op2@example.test")

	_, err := resolveCollectionOwner(ctx, q, "")
	require.Error(t, err)
}

func TestResolveCollectionOwnerExplicit(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	id := uuid.New()

	got, err := resolveCollectionOwner(ctx, q, id.String())
	require.NoError(t, err)
	require.Equal(t, id, got)
}
