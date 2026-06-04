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
	"time"

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
