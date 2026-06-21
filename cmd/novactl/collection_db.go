package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
)

// collectionParams is the validated input to createCollection.
type collectionParams struct {
	OwnerID        uuid.UUID
	Name           string
	Slug           string
	Visibility     string // public | unlisted | private
	PublicArchival bool
}

// createCollection inserts a collection owned by OwnerID. The DB enforces the
// (owner_id, slug) uniqueness and the public_archival-requires-public CHECK.
func createCollection(ctx context.Context, q *gen.Queries, p collectionParams) (gen.Collection, error) {
	return q.CreateCollection(ctx, gen.CreateCollectionParams{
		OwnerID:        pgtype.UUID{Bytes: p.OwnerID, Valid: true},
		Name:           p.Name,
		Slug:           p.Slug,
		Visibility:     gen.CollectionVisibility(p.Visibility),
		PublicArchival: p.PublicArchival,
	})
}

// resolveCollectionOwner returns the explicit owner UUID when ownerFlag is set,
// otherwise the sole operator user. It errors when ownerFlag is unparseable, or
// when no/multiple operator users exist and no owner was given.
func resolveCollectionOwner(ctx context.Context, q *gen.Queries, ownerFlag string) (uuid.UUID, error) {
	if ownerFlag != "" {
		id, err := uuid.Parse(ownerFlag)
		if err != nil {
			return uuid.Nil, fmt.Errorf("invalid --owner: %w", err)
		}
		return id, nil
	}
	ids, err := q.ListUserIDsByRole(ctx, gen.UserRoleOperator)
	if err != nil {
		return uuid.Nil, err
	}
	switch len(ids) {
	case 0:
		return uuid.Nil, errors.New("no operator user found; pass --owner <user-uuid>")
	case 1:
		return uuid.UUID(ids[0].Bytes), nil
	default:
		return uuid.Nil, errors.New("multiple operator users; pass --owner <user-uuid>")
	}
}
