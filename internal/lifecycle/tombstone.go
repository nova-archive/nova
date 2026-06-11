// Package lifecycle owns the owner/operator content-lifecycle that converges on
// the same irreversible end state as a moderation takedown — tombstoned, DEK
// crypto-shredded, derivatives cascaded, signed-URL revoked, Kubo unpinned — but
// is semantically distinct from moderation: it carries no rule, DMCA case, or
// repeat-infringer strike, and writes its own (blob.*) audit vocabulary.
//
// The neutral, irreversible destruction is TombstoneTree, shared with
// internal/moderation so the crypto-shred lives in exactly one place. Owner
// soft-delete (SoftDelete) and the overdue-soft-delete sweep (Sweeper) build on
// it. See docs/superpowers/specs/phase1/2026-06-04-phase1-m11-admin-spa-design.md
// § "The content-lifecycle primitive".
package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nova-archive/nova/internal/db/gen"
)

// CascadeHook propagates a parent blob's new state to its derivatives inside the
// same tx. The coordinator wires this to product.OnDelete; lifecycle never
// imports the product package (the dependency is inverted, like storage.WriteHook
// and moderation's cascade). moderation.CascadeHook aliases this type.
type CascadeHook func(ctx context.Context, tx pgx.Tx, parentCID, newState string) error

// Backend is the narrow subset of ipfs.Backend the unpin path needs (kept small
// so tests can supply a recording fake). moderation.Backend aliases this type.
type Backend interface {
	Unpin(ctx context.Context, c cid.Cid) error
}

// zeros72 matches data_encryption_keys.wrapped_key width (the M7 shred pattern):
// wrap_nonce(24) || ct_of_key(32) || tag(16) = 72 bytes.
var zeros72 = make([]byte, 72)

var (
	// ErrLegalHold is returned when TombstoneTree's crypto-shred is refused
	// because a target DEK carries legal_hold=true — the no_shred_under_legal_hold
	// CHECK (Postgres SQLSTATE 23514). The caller's tx rolls back; nothing is
	// tombstoned. The DB is the enforcement boundary.
	ErrLegalHold = errors.New("lifecycle: blocked by legal hold")

	// ErrNotActive is returned by SoftDelete when the targeted blob is absent or
	// not in the active state (already soft-deleted, quarantined, or tombstoned).
	ErrNotActive = errors.New("lifecycle: blob is not active")
)

// TombstoneTree performs the neutral, irreversible destruction of a blob tree
// inside an existing tx, shared by moderation takedown and owner soft-delete:
// set state → tombstoned, cascade that state to derivatives, crypto-shred the
// DEK tree, and insert a ('cid', cid) signed-URL revocation. A legal-held DEK
// raises the no_shred_under_legal_hold CHECK, mapped to ErrLegalHold (the caller
// rolls the whole tx back). Callers add their own decision/audit rows and unpin
// after commit.
func TombstoneTree(ctx context.Context, q *gen.Queries, tx pgx.Tx, cascade CascadeHook, cidStr string) error {
	if err := q.SetBlobState(ctx, gen.SetBlobStateParams{Cid: cidStr, State: gen.BlobStateTombstoned}); err != nil {
		return fmt.Errorf("lifecycle: set state: %w", err)
	}
	if cascade != nil {
		if err := cascade(ctx, tx, cidStr, string(gen.BlobStateTombstoned)); err != nil {
			return fmt.Errorf("lifecycle: cascade: %w", err)
		}
	}
	if err := q.ShredDEKsForBlobTree(ctx, gen.ShredDEKsForBlobTreeParams{Cid: cidStr, Zeros: zeros72}); err != nil {
		if isLegalHoldViolation(err) {
			return ErrLegalHold
		}
		return fmt.Errorf("lifecycle: shred: %w", err)
	}
	if err := q.InsertRevocation(ctx, gen.InsertRevocationParams{Kind: "cid", Value: cidStr}); err != nil {
		return fmt.Errorf("lifecycle: revoke: %w", err)
	}
	return nil
}

// isLegalHoldViolation reports the no_shred_under_legal_hold CHECK (SQLSTATE
// 23514). TombstoneTree maps it to ErrLegalHold and the caller rolls back.
func isLegalHoldViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "no_shred_under_legal_hold"
}
