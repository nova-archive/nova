// Package migrations holds the embed-driven forward-only Postgres
// migrations for Nova. The migrations are loaded by cmd/migrate via
// goose; tests load them directly through the exported Migrations
// embed.FS.
//
// Migrations are append-only. Down migrations exist for completeness
// but production runbooks treat schema rollback as a restore-from-
// backup operation; the forward sequence is the only supported path.
package migrations

import "embed"

//go:embed *.sql
var Migrations embed.FS
