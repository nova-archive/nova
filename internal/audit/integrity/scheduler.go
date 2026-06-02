package integrity

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nova-archive/nova/internal/db/gen"
)

const (
	// defaultTick is how often the scheduler re-evaluates per-kind due-ness.
	defaultTick = 10 * time.Second
	// defaultRunBudget caps a single check run, decoupled from its cadence, so a
	// hung Kubo call or pathological sample cannot pin memory or starve the
	// process. The read-heavy audit loop trades the job system's leasing for
	// this in-process bound.
	defaultRunBudget = 5 * time.Minute
)

// Scheduler runs the integrity checks in-process on per-kind cadences. It is
// NOT backed by the persistent job queue: there is no in-flight queue, and on
// restart it resumes from each kind's last recorded audit (INTEGRITY_AUDIT.md
// § "Restart behaviour").
type Scheduler struct {
	checks   map[Kind]Check
	cadences map[Kind]Cadence
	rec      *Recorder
	q        *gen.Queries
	log      *slog.Logger
	now      func() time.Time
	tick     time.Duration
	budget   time.Duration

	mu      sync.Mutex
	lastRun map[Kind]time.Time
	running map[Kind]bool
}

// SchedulerOption tunes a Scheduler (primarily for tests).
type SchedulerOption func(*Scheduler)

// WithClock overrides the time source.
func WithClock(now func() time.Time) SchedulerOption {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// WithTick overrides the scheduler tick interval.
func WithTick(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		if d > 0 {
			s.tick = d
		}
	}
}

// WithRunBudget overrides the per-run timeout.
func WithRunBudget(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		if d > 0 {
			s.budget = d
		}
	}
}

// NewScheduler builds a Scheduler over the given checks, cadences, recorder, and
// queries (the queries are used only to seed the resume schedule).
func NewScheduler(checks map[Kind]Check, cadences map[Kind]Cadence, rec *Recorder, q *gen.Queries, log *slog.Logger, opts ...SchedulerOption) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	s := &Scheduler{
		checks:   checks,
		cadences: cadences,
		rec:      rec,
		q:        q,
		log:      log,
		now:      time.Now,
		tick:     defaultTick,
		budget:   defaultRunBudget,
		lastRun:  make(map[Kind]time.Time),
		running:  make(map[Kind]bool),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Run seeds the schedule from the last recorded audit per kind, then ticks
// until ctx is cancelled, launching each due kind in its own goroutine.
func (s *Scheduler) Run(ctx context.Context) {
	s.seed(ctx)
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, k := range AllKinds {
				if s.due(k) {
					go s.runKind(ctx, k)
				}
			}
		}
	}
}

// seed loads MAX(audited_at) per kind so a restart resumes mid-cadence instead
// of firing every kind at boot. Best-effort: a failure leaves lastRun zero
// (everything immediately due), which is safe.
func (s *Scheduler) seed(ctx context.Context) {
	rows, err := s.q.SeedAuditSchedule(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "integrity: seed schedule", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		s.lastRun[Kind(r.AuditKind)] = r.LastRun
	}
}

// due reports whether kind k should run now: it is enabled (cadence interval
// > 0), has a registered check, is not already running, and its interval has
// elapsed since its last run.
func (s *Scheduler) due(k Kind) bool {
	c, ok := s.cadences[k]
	if !ok || c.Interval <= 0 {
		return false
	}
	if _, ok := s.checks[k]; !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[k] {
		return false
	}
	return s.now().Sub(s.lastRun[k]) >= c.Interval
}

// runKind executes one kind under the no-overlap guard, recording the run time.
func (s *Scheduler) runKind(ctx context.Context, k Kind) {
	s.mu.Lock()
	if s.running[k] {
		s.mu.Unlock()
		return
	}
	s.running[k] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running[k] = false
		s.lastRun[k] = s.now()
		s.mu.Unlock()
	}()
	s.execute(ctx, k)
}

func (s *Scheduler) execute(ctx context.Context, k Kind) {
	check, ok := s.checks[k]
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()
	findings, err := check.Run(cctx, s.cadences[k].SampleSize)
	if err != nil {
		s.log.WarnContext(cctx, "integrity: check run", "audit_kind", string(k), "err", err)
		return
	}
	if err := s.rec.Record(cctx, k, findings); err != nil {
		s.log.WarnContext(cctx, "integrity: record", "audit_kind", string(k), "err", err)
	}
}

// RunOnce runs every enabled kind once, synchronously. Used by tests and a
// future run-now action; it ignores cadence due-ness.
func (s *Scheduler) RunOnce(ctx context.Context) {
	for _, k := range AllKinds {
		if c, ok := s.cadences[k]; ok && c.Interval > 0 {
			s.runKind(ctx, k)
		}
	}
}

// RunKind runs one kind once, synchronously, ignoring cadence due-ness (but
// honoring the no-overlap guard).
func (s *Scheduler) RunKind(ctx context.Context, k Kind) {
	s.runKind(ctx, k)
}
