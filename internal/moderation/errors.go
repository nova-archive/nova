// Package moderation owns the write side of the blob_state lifecycle the M3
// read path already understands: quarantine, tombstone (with crypto-shred and
// derivative cascade), clear-legal-hold, restore, and counter-notice. Each
// operation runs in a single pgx.Tx and writes its audit_log row via
// auditlog.Writer.WriteTx inside that tx, so the action and its audit record
// commit or roll back together.
//
// See docs/superpowers/specs/2026-06-02-phase1-m9-moderation-design.md and the
// normative docs/legal/DMCA_PROCEDURE.md for the transaction step order and the
// audit-action vocabulary.
package moderation

import "errors"

var (
	// ErrLegalHold is returned when a tombstone/shred is refused because a
	// target DEK carries legal_hold=true. It is mapped from the
	// no_shred_under_legal_hold CHECK (Postgres SQLSTATE 23514); the whole
	// transaction rolls back, so nothing is tombstoned. This is the
	// severe-content exit criterion: the DB is the enforcement boundary.
	ErrLegalHold = errors.New("moderation: blocked by legal hold")

	// ErrBlobNotFound is returned when the targeted CID has no blobs row.
	ErrBlobNotFound = errors.New("moderation: blob not found")

	// ErrNotQuarantined is returned by Restore when the blob is not in the
	// quarantined state (a tombstone is final; only a quarantine is reversible).
	ErrNotQuarantined = errors.New("moderation: blob is not quarantined")
)
