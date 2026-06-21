package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/nova-archive/nova/internal/db/gen"
)

// cmdCollection dispatches `novactl collection <subcommand>` (DB-direct via
// DATABASE_URL, like `novactl node`). Lets operators create the public
// collection that anchors public reads without seeding it via raw SQL.
func cmdCollection(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: novactl collection create --name <s> --slug <s> [--visibility public|unlisted|private] [--owner <uuid>] [--public-archival]")
	}
	switch args[0] {
	case "create":
		return cmdCollectionCreate(args[1:])
	default:
		return fmt.Errorf("unknown collection subcommand %q", args[0])
	}
}

func cmdCollectionCreate(args []string) error {
	fs := flag.NewFlagSet("collection create", flag.ContinueOnError)
	name := fs.String("name", "", "human-readable collection name (required)")
	slug := fs.String("slug", "", "URL slug, unique per owner (required)")
	visibility := fs.String("visibility", "public", "public|unlisted|private")
	owner := fs.String("owner", "", "owner user UUID (default: the sole operator user)")
	publicArchival := fs.Bool("public-archival", false, "opt out of envelope encryption (requires --visibility public)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *slug == "" {
		return fmt.Errorf("--name and --slug are required")
	}
	switch *visibility {
	case "public", "unlisted", "private":
	default:
		return fmt.Errorf("--visibility must be public|unlisted|private")
	}
	return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		ownerID, err := resolveCollectionOwner(ctx, q, *owner)
		if err != nil {
			return err
		}
		col, err := createCollection(ctx, q, collectionParams{
			OwnerID:        ownerID,
			Name:           *name,
			Slug:           *slug,
			Visibility:     *visibility,
			PublicArchival: *publicArchival,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "created collection %s (slug=%s visibility=%s owner=%s)\n",
			uuidString(col.ID), col.Slug, col.Visibility, uuidString(col.OwnerID))
		return nil
	})
}
