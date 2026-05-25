// Package db is the Postgres connection layer. It exposes a single
// Open() that returns a configured pgxpool, plus a small wrapper
// type so callers can be tested with interface mocks at a coarse
// granularity. Generated sqlc query code (in internal/db/gen) uses
// this pool directly.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates a pgxpool against the given DSN with Nova's defaults:
// max 16 connections (the default replication.factor.important hint),
// 30-second connection lifetime, and TLS verified when the DSN says so.
//
// The caller is responsible for calling Close() on the returned pool.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}

	cfg.MaxConns = 16
	cfg.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return pool, nil
}
