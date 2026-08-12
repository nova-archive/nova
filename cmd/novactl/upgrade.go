package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db"
)

// `novactl upgrade` — the day-2 surface (P2-M7.3).
//
//	novactl upgrade status                 what this deployment is, right now
//	novactl upgrade check   --lock ...     preflight against a verified target
//	novactl upgrade verify  --plane ...    post-upgrade evidence, one plane
//
// All three run from the TARGET nova-admin image. An N-1 deployment cannot
// evaluate an N target — it does not know what N requires — so the target's own
// binary is what reads the lock (D-M7.3-5).
//
// None of them fetches anything. T1.22 forbids a binary from reaching out for
// release metadata, so everything they know arrives from the compiled-in
// catalog or from a file the operator verified and mounted.

func cmdUpgrade(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: novactl upgrade <status|check|verify>")
	}
	switch args[0] {
	case "status":
		return cmdUpgradeStatus(args[1:])
	case "check":
		return cmdUpgradeCheck(args[1:])
	case "verify":
		return cmdUpgradeVerify(args[1:])
	default:
		return fmt.Errorf("novactl upgrade: unknown subcommand %q", args[0])
	}
}

// optionalPool opens the database if DATABASE_URL is set and reachable, and
// returns nil otherwise.
//
// `upgrade status` must work when the database is down — that is one of the
// moments an operator most wants to ask what is running — so an unreachable
// database degrades an ANSWER rather than failing the command. `upgrade check`
// and `upgrade verify` make their own, stricter decisions.
func optionalPool(ctx context.Context) *pgxpool.Pool {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return nil
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil
	}
	return pool
}

// unavailable is what every field prints when the database cannot answer. It is
// deliberately the same token the census uses for an axis the applied schema
// cannot answer: "we could not find out" is a different statement from "the
// answer is nothing", and an operator reading a status page needs to see which.
const unavailable = "unavailable"
