package integrity

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

type stubCheck struct {
	kind     Kind
	findings []Finding
	calls    *int32
}

func (c stubCheck) Kind() Kind { return c.kind }
func (c stubCheck) Run(_ context.Context, _ int) ([]Finding, error) {
	if c.calls != nil {
		atomic.AddInt32(c.calls, 1)
	}
	return c.findings, nil
}

// blockingCheck blocks until its run context is cancelled, exercising the
// per-run budget timeout.
type blockingCheck struct{ kind Kind }

func (c blockingCheck) Kind() Kind { return c.kind }
func (c blockingCheck) Run(ctx context.Context, _ int) ([]Finding, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSchedulerDueness(t *testing.T) {
	base := time.Now()
	clock := base
	s := NewScheduler(
		map[Kind]Check{KindEnvelopeDecode: stubCheck{kind: KindEnvelopeDecode}},
		map[Kind]Cadence{KindEnvelopeDecode: {Interval: time.Hour, SampleSize: 1}},
		nil, nil, nil,
		WithClock(func() time.Time { return clock }),
	)

	require.True(t, s.due(KindEnvelopeDecode), "zero lastRun ⇒ due")

	s.lastRun[KindEnvelopeDecode] = clock
	require.False(t, s.due(KindEnvelopeDecode), "just ran ⇒ not due")

	clock = base.Add(time.Hour)
	require.True(t, s.due(KindEnvelopeDecode), "interval elapsed ⇒ due")

	s.running[KindEnvelopeDecode] = true
	require.False(t, s.due(KindEnvelopeDecode), "already running ⇒ not due")
	s.running[KindEnvelopeDecode] = false

	s.cadences[KindEnvelopeDecode] = Cadence{Interval: 0, SampleSize: 1}
	require.False(t, s.due(KindEnvelopeDecode), "disabled (interval 0) ⇒ not due")

	noCheck := NewScheduler(
		map[Kind]Check{},
		map[Kind]Cadence{KindKeyUnwrap: {Interval: time.Hour, SampleSize: 1}},
		nil, nil, nil,
	)
	require.False(t, noCheck.due(KindKeyUnwrap), "no registered check ⇒ not due")
}

func TestSchedulerRunKindTimesOut(t *testing.T) {
	s := NewScheduler(
		map[Kind]Check{KindKuboPinPresent: blockingCheck{kind: KindKuboPinPresent}},
		map[Kind]Cadence{KindKuboPinPresent: {Interval: time.Hour, SampleSize: 1}},
		NewRecorder(nil, NewNoopSink(), nil), // Record never reached: the check errors out
		nil, nil,
		WithRunBudget(20*time.Millisecond),
	)
	done := make(chan struct{})
	go func() {
		s.RunKind(context.Background(), KindKuboPinPresent)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunKind did not return; per-run budget timeout not enforced")
	}
}

func TestSchedulerWithDB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scheduler DB test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)

	t.Run("RunOnce runs enabled kinds and records findings", func(t *testing.T) {
		var calls int32
		s := NewScheduler(
			map[Kind]Check{KindEnvelopeDecode: stubCheck{
				kind:     KindEnvelopeDecode,
				findings: []Finding{{CID: "bafyRunOnce", Result: ResultPass}},
				calls:    &calls,
			}},
			map[Kind]Cadence{KindEnvelopeDecode: {Interval: time.Hour, SampleSize: 1}},
			NewRecorder(q, NewNoopSink(), nil), q, nil,
		)
		s.RunOnce(ctx)
		require.EqualValues(t, 1, atomic.LoadInt32(&calls))

		n, err := q.CountIntegrityAudits(ctx, gen.CountIntegrityAuditsParams{
			AuditKind: gen.NullAuditKind{AuditKind: gen.AuditKindEnvelopeDecode, Valid: true},
		})
		require.NoError(t, err)
		require.GreaterOrEqual(t, n, int64(1))
	})

	t.Run("seed defers a recently-audited kind", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO integrity_audits (cid, audit_kind, result) VALUES ('bafySeed','key_unwrap','pass')`)
		require.NoError(t, err)
		s := NewScheduler(
			map[Kind]Check{KindKeyUnwrap: stubCheck{kind: KindKeyUnwrap}},
			map[Kind]Cadence{KindKeyUnwrap: {Interval: time.Hour, SampleSize: 1}},
			NewRecorder(q, NewNoopSink(), nil), q, nil,
		)
		s.seed(ctx)
		require.False(t, s.due(KindKeyUnwrap), "recent audit ⇒ seeded lastRun ⇒ not due")
	})
}
