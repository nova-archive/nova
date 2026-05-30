// Package imagemeta reads and writes the core-owned image_metadata side table.
// perceptual_hash is written NULL in Phase 1 (the Go-native pHash dedup signal
// is Phase 3; PDQ for external matching is Phase 4 — see the M5 spec).
package imagemeta

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Insert writes the image_metadata row inside an existing transaction (so it
// commits atomically with the blob row). perceptual_hash is NULL in Phase 1.
func Insert(ctx context.Context, tx pgx.Tx, cid string, width, height int, alt, caption *string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO image_metadata (cid, width, height, perceptual_hash, alt_text, caption)
		 VALUES ($1, $2, $3, NULL, $4, $5)
		 ON CONFLICT (cid) DO NOTHING`, cid, width, height, alt, caption)
	return err
}

// Meta is the public image metadata for the /i/{cid}.json route.
type Meta struct {
	Width   int
	Height  int
	AltText *string
	Caption *string
}

// Get reads the image_metadata row for cid (pgx.ErrNoRows if absent).
func Get(ctx context.Context, pool *pgxpool.Pool, cid string) (Meta, error) {
	var m Meta
	err := pool.QueryRow(ctx,
		`SELECT width, height, alt_text, caption FROM image_metadata WHERE cid = $1`, cid).
		Scan(&m.Width, &m.Height, &m.AltText, &m.Caption)
	return m, err
}
