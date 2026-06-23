package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/ca"
)

// withNodeDB opens a pool from DATABASE_URL and runs fn with queries.
func withNodeDB(fn func(ctx context.Context, q *gen.Queries) error) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL must be set for node registry commands")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return fn(ctx, gen.New(pool))
}

func withNodeDBPool(fn func(ctx context.Context, pool *pgxpool.Pool) error) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL must be set for pin commands")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return fn(ctx, pool)
}

func parsePGUUID(s string) (pgtype.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid --id: %w", err)
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

// revokeNode is the testable core: it flips status to revoked.
func revokeNode(ctx context.Context, q *gen.Queries, pgID pgtype.UUID) error {
	n, err := q.RevokeNode(ctx, pgID)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("node not found or already revoked")
	}
	return nil
}

func cmdNodeRevoke(args []string) error {
	fs := flag.NewFlagSet("node revoke", flag.ContinueOnError)
	idStr := fs.String("id", "", "node id (uuid)")
	noConfirm := fs.Bool("no-confirm", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pgID, err := parsePGUUID(*idStr)
	if err != nil {
		return err
	}
	if !*noConfirm {
		fmt.Printf("Revoke node %s? Its certificate will be refused at the next request. [y/N]: ", *idStr)
		var ans string
		fmt.Scanln(&ans)
		if ans != "y" && ans != "Y" {
			return errors.New("aborted")
		}
	}
	return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		if err := revokeNode(ctx, q, pgID); err != nil {
			return err
		}
		fmt.Printf("revoked node %s\n", *idStr)
		return nil
	})
}

// rotateNodeCert is the testable core: it swaps the stored fingerprint to newFP.
func rotateNodeCert(ctx context.Context, q *gen.Queries, pgID pgtype.UUID, newFP string) error {
	n, err := q.RotateNodeCert(ctx, gen.RotateNodeCertParams{ID: pgID, FederationCertFingerprint: newFP})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("node not found")
	}
	return nil
}

func cmdNodeRotateCert(args []string) error {
	fs := flag.NewFlagSet("node rotate-cert", flag.ContinueOnError)
	idStr := fs.String("id", "", "node id (uuid)")
	dir := fs.String("dir", ".", "directory holding federation-ca.crt + federation-ca.key")
	name := fs.String("name", "donor", "donor display name for the new cert")
	out := fs.String("out", "", "output dir for the replacement bundle (default ./<id>-rotated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := uuid.Parse(*idStr)
	if err != nil {
		return fmt.Errorf("invalid --id: %w", err)
	}
	caCertPEM, err := os.ReadFile(filepath.Join(*dir, "federation-ca.crt"))
	if err != nil {
		return err
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(*dir, "federation-ca.key"))
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := ca.IssueClientCert(caCertPEM, caKeyPEM, id, *name) // SAME node_id
	if err != nil {
		return err
	}
	outDir := *out
	if outDir == "" {
		outDir = filepath.Join(".", *idStr+"-rotated")
	}
	for fn, data := range map[string][]byte{"federation.crt": certPEM, "federation.key": keyPEM, "federation-ca.crt": caCertPEM} {
		perm := os.FileMode(0o644)
		if filepath.Ext(fn) == ".key" {
			perm = 0o600
		}
		if err := writeNodeFile(filepath.Join(outDir, fn), data, perm); err != nil {
			return err
		}
	}
	newFP := leafFingerprint(certPEM)
	if err := withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		return rotateNodeCert(ctx, q, pgtype.UUID{Bytes: id, Valid: true}, newFP)
	}); err != nil {
		return err
	}
	fmt.Printf("rotated node %s — new fingerprint %s active (downtime cutover until donor restarts with %s)\n", *idStr, newFP, outDir)
	return nil
}

func cmdNodeList(args []string) error {
	return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		rows, err := q.ListNodes(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-38s %-16s %-12s %-12s %s\n", "NODE_ID", "DISPLAY", "STATUS", "TRUST", "LAST_SEEN")
		for _, r := range rows {
			fmt.Printf("%-38s %-16s %-12s %-12s %v\n", uuidString(r.ID), r.DisplayName.String, r.Status, r.TrustState, r.LastSeenAt.Time)
		}
		return nil
	})
}

// uuidString renders a pgtype.UUID.
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}
