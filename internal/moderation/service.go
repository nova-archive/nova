package moderation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/lifecycle"
)

// CascadeHook propagates a parent's new blob state to its derivatives. It
// aliases lifecycle.CascadeHook so moderation takedown and owner soft-delete
// share one cascade wiring (product.OnDelete); moderation never imports the
// product package (the dependency is inverted, like storage.WriteHook).
type CascadeHook = lifecycle.CascadeHook

// Backend is the narrow subset of ipfs.Backend the tombstone path needs. It
// aliases lifecycle.Backend (shared with the soft-delete unpin path); tests can
// supply a recording fake.
type Backend = lifecycle.Backend

// Service runs the five moderation operations. Each runs in one pgx.Tx and
// writes its audit_log row via the auditlog.Writer inside that tx.
type Service struct {
	q       *gen.Queries
	pool    *pgxpool.Pool
	backend Backend
	cascade CascadeHook
	audit   *auditlog.Writer
	log     *slog.Logger
	now     func() time.Time
}

// NewService builds a Service. A nil cascade becomes a no-op; a nil clock
// becomes time.Now.
func NewService(q *gen.Queries, pool *pgxpool.Pool, b Backend, c CascadeHook, a *auditlog.Writer, log *slog.Logger, now func() time.Time) *Service {
	if c == nil {
		c = func(context.Context, pgx.Tx, string, string) error { return nil }
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{q: q, pool: pool, backend: b, cascade: c, audit: a, log: log, now: now}
}

// QuarantineCmd describes a quarantine action.
type QuarantineCmd struct {
	CID, Rule, RuleRef, Reason string
	TombstoneAfter             time.Duration
	LegalHold                  bool
	Actor                      *uuid.UUID
}

// Quarantine blocks reads and revokes signed URLs for a CID while preserving the
// bytes through the counter-notification window. When LegalHold is set it also
// flips the DEK tree's legal_hold and leaves scheduled_tombstone_at NULL, so the
// no_shred_under_legal_hold CHECK refuses any later tombstone until cleared.
func (s *Service) Quarantine(ctx context.Context, cmd QuarantineCmd) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	info, err := q.GetBlobForModeration(ctx, cmd.CID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBlobNotFound
	}
	if err != nil {
		return fmt.Errorf("moderation: get blob: %w", err)
	}

	var sched pgtype.Timestamptz
	if !cmd.LegalHold {
		sched = pgtype.Timestamptz{Time: s.now().Add(cmd.TombstoneAfter), Valid: true}
	}
	if _, err := q.InsertModerationDecision(ctx, gen.InsertModerationDecisionParams{
		Cid:                  cmd.CID,
		Rule:                 gen.ModerationRule(orDefault(cmd.Rule, "operator_manual")),
		RuleRef:              text(cmd.RuleRef),
		Action:               gen.ModerationActionQuarantine,
		DecidedBy:            uuidPg(cmd.Actor),
		ScheduledTombstoneAt: sched,
		LegalHold:            cmd.LegalHold,
		Notes:                text(cmd.Reason),
	}); err != nil {
		return fmt.Errorf("moderation: insert decision: %w", err)
	}

	if err := q.SetBlobState(ctx, gen.SetBlobStateParams{Cid: cmd.CID, State: gen.BlobStateQuarantined}); err != nil {
		return fmt.Errorf("moderation: set state: %w", err)
	}
	if err := s.cascade(ctx, tx, cmd.CID, string(gen.BlobStateQuarantined)); err != nil {
		return fmt.Errorf("moderation: cascade: %w", err)
	}

	if cmd.LegalHold {
		if err := q.SetDEKLegalHoldForBlobTree(ctx, gen.SetDEKLegalHoldForBlobTreeParams{Cid: cmd.CID, Hold: true}); err != nil {
			return fmt.Errorf("moderation: set legal hold: %w", err)
		}
	}
	if err := q.InsertRevocation(ctx, gen.InsertRevocationParams{Kind: "cid", Value: cmd.CID}); err != nil {
		return fmt.Errorf("moderation: insert revocation: %w", err)
	}
	if err := actionCaseRef(ctx, q, cmd.RuleRef); err != nil {
		return err
	}
	if err := strikeOwner(ctx, q, info.OwnerID); err != nil {
		return err
	}

	action := "dmca.quarantined"
	if cmd.LegalHold {
		action = "severe.quarantined"
	}
	if err := s.audit.WriteTx(ctx, tx, auditlog.Entry{
		ActorID: cmd.Actor, Action: action, TargetType: "cid", TargetID: cmd.CID,
		Payload: map[string]any{
			"reason":     cmd.Reason,
			"case":       cmd.RuleRef,
			"legal_hold": cmd.LegalHold,
		},
	}); err != nil {
		return fmt.Errorf("moderation: audit: %w", err)
	}
	return tx.Commit(ctx)
}

// TombstoneCmd describes a tombstone action (manual takedown or the sweep).
type TombstoneCmd struct {
	CID, Rule, RuleRef, Reason string
	Actor                      *uuid.UUID
}

// Tombstone makes a CID's bytes permanently unrecoverable: it crypto-shreds the
// parent + derivative DEKs, transitions state to tombstoned, cascades that state
// to derivatives, inserts a revocation, and (after commit) best-effort unpins
// the parent and each derivative. If any target DEK carries legal_hold=true the
// no_shred_under_legal_hold CHECK raises (SQLSTATE 23514); it is mapped to
// ErrLegalHold and the whole transaction rolls back — nothing is tombstoned.
func (s *Service) Tombstone(ctx context.Context, cmd TombstoneCmd) error {
	// Collect derivative CIDs before the tx so the post-commit unpin loop has
	// them even though the cascade/shred are set-based inside the tx.
	derivs, err := s.q.ListDerivativeCIDs(ctx, pgtype.Text{String: cmd.CID, Valid: true})
	if err != nil {
		return fmt.Errorf("moderation: list derivatives: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if _, err := q.InsertModerationDecision(ctx, gen.InsertModerationDecisionParams{
		Cid:       cmd.CID,
		Rule:      gen.ModerationRule(orDefault(cmd.Rule, "operator_manual")),
		RuleRef:   text(cmd.RuleRef),
		Action:    gen.ModerationActionTombstone,
		DecidedBy: uuidPg(cmd.Actor),
		LegalHold: false,
		Notes:     text(cmd.Reason),
	}); err != nil {
		return fmt.Errorf("moderation: insert decision: %w", err)
	}

	// Evict the originating quarantine row from the sweep's partial index.
	if err := q.ClearScheduledTombstone(ctx, cmd.CID); err != nil {
		return fmt.Errorf("moderation: clear schedule: %w", err)
	}
	// The neutral, irreversible destruction (state→tombstoned, cascade to
	// derivatives, crypto-shred the DEK tree, revoke) is shared with owner
	// soft-delete via lifecycle.TombstoneTree. The DB CHECK is the legal-hold
	// enforcement boundary, surfaced here as ErrLegalHold (deferred Rollback
	// undoes everything).
	if err := lifecycle.TombstoneTree(ctx, q, tx, s.cascade, cmd.CID); err != nil {
		if errors.Is(err, lifecycle.ErrLegalHold) {
			return ErrLegalHold
		}
		return err
	}
	if err := actionCaseRef(ctx, q, cmd.RuleRef); err != nil {
		return err
	}
	info, err := q.GetBlobForModeration(ctx, cmd.CID)
	if err != nil {
		return fmt.Errorf("moderation: get blob: %w", err)
	}
	if err := strikeOwner(ctx, q, info.OwnerID); err != nil {
		return err
	}

	if err := s.audit.WriteTx(ctx, tx, auditlog.Entry{
		ActorID: cmd.Actor, Action: "dmca.tombstoned", TargetType: "cid", TargetID: cmd.CID,
		Payload: map[string]any{"reason": cmd.Reason, "case": cmd.RuleRef},
	}); err != nil {
		return fmt.Errorf("moderation: audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// Best-effort, idempotent, after commit — the bytes are already inert.
	s.unpin(ctx, cmd.CID)
	for _, d := range derivs {
		s.unpin(ctx, d)
	}
	return nil
}

// ClearLegalHold releases a severe-content hold (operator-only at the HTTP
// layer; not enforced here): it flips the DEK tree's legal_hold to false and
// sets the originating decision's legal_hold=false, scheduled_tombstone_at=now()
// so the next sweep tombstones it. This is the SEVERE_CONTENT_PROCEDURE.md
// "Operator legal-hold-clear" procedure.
func (s *Service) ClearLegalHold(ctx context.Context, cidStr, caseRef, reason string, actor *uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := q.SetDEKLegalHoldForBlobTree(ctx, gen.SetDEKLegalHoldForBlobTreeParams{Cid: cidStr, Hold: false}); err != nil {
		return fmt.Errorf("moderation: clear dek legal hold: %w", err)
	}
	if err := q.ClearModerationLegalHold(ctx, cidStr); err != nil {
		return fmt.Errorf("moderation: clear decision legal hold: %w", err)
	}
	if err := s.audit.WriteTx(ctx, tx, auditlog.Entry{
		ActorID: actor, Action: "severe.legal_hold_cleared", TargetType: "cid", TargetID: cidStr,
		Payload: map[string]any{"reason": reason, "case": caseRef},
	}); err != nil {
		return fmt.Errorf("moderation: audit: %w", err)
	}
	return tx.Commit(ctx)
}

// Restore reverses a quarantine (a tombstone is final): it sets the blob and its
// derivatives back to active and clears the schedule. It deliberately does NOT
// delete the ('cid', cid) revocation — signed_url_revocations has no issuer
// column, so a blind delete could clobber a manual M7 operator revocation;
// re-minting after restore is the intended posture (design review decision).
func (s *Service) Restore(ctx context.Context, cidStr, reason string, actor *uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	info, err := q.GetBlobForModeration(ctx, cidStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBlobNotFound
	}
	if err != nil {
		return fmt.Errorf("moderation: get blob: %w", err)
	}
	if info.State != string(gen.BlobStateQuarantined) {
		return ErrNotQuarantined
	}

	if err := q.SetBlobState(ctx, gen.SetBlobStateParams{Cid: cidStr, State: gen.BlobStateActive}); err != nil {
		return fmt.Errorf("moderation: set state: %w", err)
	}
	if err := s.cascade(ctx, tx, cidStr, string(gen.BlobStateActive)); err != nil {
		return fmt.Errorf("moderation: cascade: %w", err)
	}
	if err := q.ClearScheduledTombstone(ctx, cidStr); err != nil {
		return fmt.Errorf("moderation: clear schedule: %w", err)
	}
	if err := s.audit.WriteTx(ctx, tx, auditlog.Entry{
		ActorID: actor, Action: "dmca.restored", TargetType: "cid", TargetID: cidStr,
		Payload: map[string]any{"reason": reason},
	}); err != nil {
		return fmt.Errorf("moderation: audit: %w", err)
	}
	return tx.Commit(ctx)
}

// CounterNotice pauses the scheduled tombstone while a counter-notification is
// reviewed. The blob stays quarantined (reversibility preserved through the
// window); the operator later Restores it or lets a renewed schedule tombstone.
func (s *Service) CounterNotice(ctx context.Context, cidStr, notes string, actor *uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := q.ClearScheduledTombstone(ctx, cidStr); err != nil {
		return fmt.Errorf("moderation: clear schedule: %w", err)
	}
	if err := s.audit.WriteTx(ctx, tx, auditlog.Entry{
		ActorID: actor, Action: "dmca.counter_received", TargetType: "cid", TargetID: cidStr,
		Payload: map[string]any{"notes": notes},
	}); err != nil {
		return fmt.Errorf("moderation: audit: %w", err)
	}
	return tx.Commit(ctx)
}

// unpin removes the local Kubo pin for cidStr, best-effort. Decode/Unpin
// failures are logged, never fatal (Phase 1 is single-node; the federation
// unpin broadcast lands with the mesh in Phase 2).
func (s *Service) unpin(ctx context.Context, cidStr string) {
	if s.backend == nil {
		return
	}
	c, err := cid.Decode(cidStr)
	if err != nil {
		s.log.Warn("moderation: bad cid for unpin", "cid", cidStr, "err", err)
		return
	}
	if err := s.backend.Unpin(ctx, c); err != nil {
		s.log.Warn("moderation: unpin", "cid", cidStr, "err", err)
	}
}

// --- shared helpers ----------------------------------------------------------

// actionCaseRef advances a referenced DMCA case to 'actioned' when ruleRef
// names a UUID. A non-UUID ruleRef (e.g. a free-form note) is ignored.
func actionCaseRef(ctx context.Context, q *gen.Queries, ruleRef string) error {
	if ruleRef == "" {
		return nil
	}
	id, perr := uuid.Parse(ruleRef)
	if perr != nil {
		return nil
	}
	if err := q.SetDMCACaseActioned(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		return fmt.Errorf("moderation: action case: %w", err)
	}
	return nil
}

// strikeOwner increments the owner's repeat-infringer strike. ownerID is the
// owner_id::text projection from GetBlobForModeration; an empty/invalid value
// (a blob with no owner) is skipped.
func strikeOwner(ctx context.Context, q *gen.Queries, ownerID string) error {
	if ownerID == "" {
		return nil
	}
	oid, perr := uuid.Parse(ownerID)
	if perr != nil {
		return nil
	}
	if err := q.UpsertRepeatInfringer(ctx, pgtype.UUID{Bytes: oid, Valid: true}); err != nil {
		return fmt.Errorf("moderation: strike: %w", err)
	}
	return nil
}

func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func uuidPg(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
