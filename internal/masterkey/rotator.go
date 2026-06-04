// Package masterkey implements operator master-key rotation: re-wrapping every
// per-blob DEK and signing key from a retiring master-key version to the active
// one, online and in parallel, with no read-path downtime. See
// docs/specs/ENCRYPTION_ENVELOPE.md and docs/superpowers/specs/2026-06-03-phase1-m10-master-key-rotation-design.md.
package masterkey

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
)

var (
	ErrToNotActive     = errors.New("masterkey: to_version must equal the active master-key label (set NOVA_MASTER_KEY_ACTIVE and restart first)")
	ErrInvalidFrom     = errors.New("masterkey: from_version must be a loaded, non-active, non-retired version")
	ErrAlreadyRotating = errors.New("masterkey: a rotation is already in progress")
)

// Config carries the Rotator's dependencies and tunables.
type Config struct {
	Q               *gen.Queries
	Pool            *pgxpool.Pool
	Keystore        *envelope.Keystore
	Audit           *auditlog.Writer // best-effort; nil ⇒ no audit
	Logger          *slog.Logger
	Concurrency     int           // DEK worker goroutines; <=0 ⇒ 4
	BatchSize       int           // ids claimed per tx; <=0 ⇒ 256
	Pace            time.Duration // sleep between batch commits; <=0 ⇒ none
	Now             func() time.Time
	OnSigningRewrap func() // invoked after signing keys re-wrapped (defensive cache invalidation); nil ⇒ skipped
}

type job struct{ from, to string }

// Rotator drives master-key rotation. Run it once via Run(ctx) (Task 4); trigger
// rotations via Start; observe via Status/Readyz (Task 5).
type Rotator struct {
	q       *gen.Queries
	pool    *pgxpool.Pool
	ks      *envelope.Keystore
	audit   *auditlog.Writer
	log     *slog.Logger
	conc    int
	batch   int
	pace    time.Duration
	now     func() time.Time
	onSign  func()
	trigger chan job

	// test seam (Task 3): invoked with the transient plaintext key right after
	// re-wrap, before zeroing, so a test can assert the buffer is zeroed.
	captureKey func([]byte)
}

func NewRotator(c Config) *Rotator {
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 256
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Rotator{
		q: c.Q, pool: c.Pool, ks: c.Keystore, audit: c.Audit, log: c.Logger,
		conc: c.Concurrency, batch: c.BatchSize, pace: c.Pace, now: c.Now,
		onSign: c.OnSigningRewrap, trigger: make(chan job, 1),
	}
}

// Start validates a rotation, atomically marks the source version 'rotating',
// and enqueues the drain (which Run executes in Task 4). Non-blocking.
func (r *Rotator) Start(ctx context.Context, from, to string) error {
	if to != r.ks.ActiveLabel() || !r.ks.HasLabel(to) {
		return ErrToNotActive
	}
	if from == to || !r.ks.HasLabel(from) {
		return ErrInvalidFrom
	}
	row, err := r.q.GetMasterVersionByLabel(ctx, from)
	if err != nil || row.State == gen.KeyStateRetired {
		return ErrInvalidFrom
	}
	n, err := r.q.BeginVersionRotation(ctx, from)
	if err != nil {
		return fmt.Errorf("masterkey: begin rotation: %w", err)
	}
	if n == 0 {
		// Either another version is already rotating, or `from` was not 'active'.
		if _, e := r.q.GetRotatingVersion(ctx); e == nil {
			return ErrAlreadyRotating
		}
		return ErrInvalidFrom
	}
	// The handler writes the actor-attributed `master_key.rotation_started` audit
	// row (Task 6); the Rotator audits completed/resumed as system actions (Task 4).
	select {
	case r.trigger <- job{from, to}:
	default: // a drain is already queued/running; the DB 'rotating' guard is authoritative
	}
	return nil
}

// zero overwrites b with zeros (best-effort transient-secret hygiene; used in Task 3).
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// SetCaptureKeyForTest installs the zeroing test seam. Test-only.
func (r *Rotator) SetCaptureKeyForTest(fn func([]byte)) { r.captureKey = fn }

// drainDEKs re-wraps every active/rotating DEK for `from` to the active version
// using r.conc parallel workers. Returns when drained or ctx ends / a worker errs.
func (r *Rotator) drainDEKs(ctx context.Context, from string) error {
	fromUUID, ok := r.ks.VersionID(from)
	if !ok {
		return fmt.Errorf("masterkey: version id for %q not cached", from)
	}
	fromID := pgtype.UUID{Bytes: fromUUID, Valid: true}
	var wg sync.WaitGroup
	errc := make(chan error, r.conc)
	for i := 0; i < r.conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				n, err := r.drainBatch(ctx, fromID)
				if err != nil {
					errc <- err
					return
				}
				if n == 0 {
					return
				}
				if r.pace > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(r.pace):
					}
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errc:
		return err
	default:
		return nil
	}
}

// drainBatch claims and re-wraps up to r.batch DEKs in one transaction. The
// RewrapDEK guard (WHERE master_key_version_id = old) makes it idempotent and
// race-safe; wrapped_key + version flip atomically.
func (r *Rotator) drainBatch(ctx context.Context, fromID pgtype.UUID) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := r.q.WithTx(tx)

	rows, err := q.ClaimDEKsForRewrap(ctx, gen.ClaimDEKsForRewrapParams{
		MasterKeyVersionID: fromID, Limit: int32(r.batch),
	})
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	for _, row := range rows {
		pbk, err := r.ks.Unwrap(ctx, row.WrappedKey, uuid.UUID(fromID.Bytes))
		if err != nil {
			return 0, fmt.Errorf("masterkey: unwrap dek %x: %w", row.ID.Bytes, err)
		}
		wrapped, toUUID, err := r.ks.Wrap(pbk)
		if r.captureKey != nil {
			r.captureKey(pbk)
		}
		zero(pbk)
		if err != nil {
			return 0, fmt.Errorf("masterkey: wrap dek %x: %w", row.ID.Bytes, err)
		}
		if _, err := q.RewrapDEK(ctx, gen.RewrapDEKParams{
			WrappedKey:   wrapped,
			NewVersionID: pgtype.UUID{Bytes: toUUID, Valid: true},
			ID:           row.ID,
			OldVersionID: fromID,
		}); err != nil {
			return 0, fmt.Errorf("masterkey: rewrap dek %x: %w", row.ID.Bytes, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// rewrapSigningKeys re-wraps active + within-grace retired signing keys for
// `from` to the active version. Few rows; no batching required.
func (r *Rotator) rewrapSigningKeys(ctx context.Context, from string) error {
	fromUUID, ok := r.ks.VersionID(from)
	if !ok {
		return fmt.Errorf("masterkey: version id for %q not cached", from)
	}
	fromID := pgtype.UUID{Bytes: fromUUID, Valid: true}
	rows, err := r.q.ListSigningKeysForRewrap(ctx, fromID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		secret, err := r.ks.Unwrap(ctx, row.WrappedKey, fromUUID)
		if err != nil {
			return fmt.Errorf("masterkey: unwrap signing key %s: %w", row.Kid, err)
		}
		wrapped, toUUID, err := r.ks.Wrap(secret)
		zero(secret)
		if err != nil {
			return fmt.Errorf("masterkey: wrap signing key %s: %w", row.Kid, err)
		}
		if _, err := r.q.RewrapSigningKey(ctx, gen.RewrapSigningKeyParams{
			WrappedKey:   wrapped,
			NewVersionID: pgtype.UUID{Bytes: toUUID, Valid: true},
			Kid:          row.Kid,
			OldVersionID: fromID,
		}); err != nil {
			return fmt.Errorf("masterkey: rewrap signing key %s: %w", row.Kid, err)
		}
	}
	return nil
}

// drain runs a full rotation: DEKs → signing keys → retire. Stalls (returns,
// leaving the version 'rotating') if the source key is not loaded.
func (r *Rotator) drain(ctx context.Context, from, to string) {
	if !r.ks.HasLabel(from) {
		r.log.Warn("master-key rotation stalled: source key not loaded", "from", from)
		return
	}
	start := r.now()
	if err := r.drainDEKs(ctx, from); err != nil {
		r.log.Error("master-key rotation: drain DEKs", "from", from, "err", err)
		return
	}
	if err := r.rewrapSigningKeys(ctx, from); err != nil {
		r.log.Error("master-key rotation: rewrap signing keys", "from", from, "err", err)
		return
	}
	if err := r.q.RetireVersion(ctx, from); err != nil {
		r.log.Error("master-key rotation: retire version", "from", from, "err", err)
		return
	}
	if r.onSign != nil {
		r.onSign()
	}
	r.log.Info("master-key rotation completed", "from", from, "to", to, "duration", r.now().Sub(start))
	if r.audit != nil {
		r.audit.Write(ctx, auditlog.Entry{
			Action:     "master_key.rotation_completed",
			TargetType: "master_key_version",
			TargetID:   from,
			Payload:    map[string]any{"from": from, "to": to, "duration_ms": r.now().Sub(start).Milliseconds()},
		})
	}
}

// Run drives the Rotator: resume any interrupted rotation, then drain on each
// trigger until ctx is cancelled. Start exactly once (coordinator.Run, Task 7).
func (r *Rotator) Run(ctx context.Context) {
	r.resumeIfRotating(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-r.trigger:
			r.drain(ctx, j.from, j.to)
		}
	}
}

// resumeIfRotating finishes a rotation interrupted by a restart. Target is the
// current active version. No-op when nothing rotating or source == active;
// stalls (logs) when the source key is not loaded.
func (r *Rotator) resumeIfRotating(ctx context.Context) {
	row, err := r.q.GetRotatingVersion(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		r.log.Error("master-key rotation: resume probe", "err", err)
		return
	}
	from, to := row.VersionLabel, r.ks.ActiveLabel()
	if from == to {
		r.log.Warn("master-key rotation: rotating version equals active; skipping resume", "version", from)
		return
	}
	if !r.ks.HasLabel(from) {
		r.log.Warn("master-key rotation stalled on resume: source key not loaded", "from", from)
		return
	}
	if r.audit != nil {
		r.audit.Write(ctx, auditlog.Entry{
			Action:     "master_key.rotation_resumed",
			TargetType: "master_key_version",
			TargetID:   from,
			Payload:    map[string]any{"from": from, "to": to},
		})
	}
	r.drain(ctx, from, to)
}

// drainOnceForTest runs a single synchronous drain. Test-only.
func (r *Rotator) drainOnceForTest(ctx context.Context, from, to string) { r.drain(ctx, from, to) }

// --- Task 5: Status projection + Readyz stall-detection ---------------------

// VersionInfo carries a snapshot of one master-key version for the Status response.
type VersionInfo struct {
	Label        string     `json:"label"`
	State        string     `json:"state"`
	DEKCount     int64      `json:"dek_count"`
	SigningCount int64      `json:"signing_count"`
	RetiredAt    *time.Time `json:"retired_at"`
}

// Progress describes an in-progress rotation (a version in 'rotating' state).
type Progress struct {
	From                 string `json:"from"`
	RemainingDEKs        int64  `json:"remaining_deks"`
	RemainingSigningKeys int64  `json:"remaining_signing_keys"`
	Stalled              bool   `json:"stalled"`
	StallReason          string `json:"stall_reason,omitempty"`
}

// Status is the top-level rotation-status snapshot returned by Status(ctx).
type Status struct {
	Active     string        `json:"active"`
	InProgress *Progress     `json:"in_progress"`
	Versions   []VersionInfo `json:"versions"`
}

// Status returns a point-in-time snapshot of every master-key version and any
// in-progress rotation. It never mutates state.
func (r *Rotator) Status(ctx context.Context) (Status, error) {
	out := Status{Active: r.ks.ActiveLabel()}
	rows, err := r.q.ListMasterVersions(ctx)
	if err != nil {
		return out, err
	}
	for _, v := range rows {
		vi := VersionInfo{
			Label:        v.VersionLabel,
			State:        string(v.State),
			DEKCount:     v.DekCount,
			SigningCount: v.SigningCount,
		}
		if v.RetiredAt.Valid {
			t := v.RetiredAt.Time
			vi.RetiredAt = &t
		}
		out.Versions = append(out.Versions, vi)
		if v.State == gen.KeyStateRotating {
			p := &Progress{
				From:                 v.VersionLabel,
				RemainingDEKs:        v.DekCount,
				RemainingSigningKeys: v.SigningCount,
			}
			if !r.ks.HasLabel(v.VersionLabel) {
				p.Stalled = true
				p.StallReason = "source master key not loaded"
			}
			out.InProgress = p
		}
	}
	return out, nil
}

// Readyz degrades when a rotation is stalled (a 'rotating' version whose key is
// not loaded). Wire as a /readyz ReadyCheck — NOT a liveness probe; a restart
// cannot fix a missing key.
func (r *Rotator) Readyz(ctx context.Context) error {
	row, err := r.q.GetRotatingVersion(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return nil // a transient query error is not a rotation stall
	}
	if !r.ks.HasLabel(row.VersionLabel) {
		return fmt.Errorf("master-key rotation stalled: version %q key not loaded", row.VersionLabel)
	}
	return nil
}
