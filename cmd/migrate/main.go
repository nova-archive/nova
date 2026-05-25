// Package main is the Nova migration runner. It loads the embedded
// SQL files from internal/db/migrations and applies them in order
// against the database identified by DATABASE_URL.
//
// Subcommands:
//
//	migrate up                  apply all pending migrations
//	migrate down                roll back one migration
//	migrate status              show applied/pending
//	migrate version             show current version
//	migrate create <name>       create a new migration template (Phase 2+)
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/nova-archive/nova/internal/db/migrations"
	"github.com/pressly/goose/v3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}

	args := os.Args[1:]
	if len(args) == 0 {
		return errors.New("usage: migrate <up|down|status|version>")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	goose.SetBaseFS(migrations.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	switch args[0] {
	case "up":
		return goose.UpContext(ctx, db, ".")
	case "down":
		return goose.DownContext(ctx, db, ".")
	case "status":
		return goose.StatusContext(ctx, db, ".")
	case "version":
		return goose.VersionContext(ctx, db, ".")
	default:
		return fmt.Errorf("unknown subcommand: %s", args[0])
	}
}

// Make stdlib's pgx driver registration explicit so static checkers don't
// trim the side-effect import.
var _ = stdlib.GetDefaultDriver
