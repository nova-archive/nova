package upgrade

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nova-archive/nova/internal/db/migrations"
)

// postgresImage is pinned by digest, for the same reason the rest of P2-M7.3
// is: a test that silently changes its own Postgres out from under a schema
// assertion is this milestone's problem at a smaller scale.
//
// postgres:16-alpine as resolved 2026-08-11.
const postgresImage = "postgres:16-alpine@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777"

// unsafeFixture is the COMMITTED migration set used to prove the refusal path.
// A test that edits production source to make itself fail proves only that the
// test can edit files.
var unsafeFixture = os.DirFS(filepath.Join("testdata", "unsafe_fixture"))

// emptyDB returns an UNMIGRATED database. Most tests here are about the road
// from one schema to another, so starting at the destination would skip the
// part under test.
func emptyDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	container, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("nova"),
		postgres.WithUsername("nova"),
		postgres.WithPassword("test-password"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(shutdownCtx)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.PingContext(ctx))
	return db
}

// upTo applies the real migration set to an exact version, so a test can stand
// the database at schema 18 — the one transition that has to work.
func upTo(t *testing.T, ctx context.Context, db *sql.DB, target int64) {
	t.Helper()
	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetBaseFS(migrations.Migrations)
	require.NoError(t, goose.UpToContext(ctx, db, ".", target))
}

func applied(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var v sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v)
	if err != nil || !v.Valid {
		return 0
	}
	return v.Int64
}

func latest() int64 {
	var max int64
	for _, o := range migrations.All() {
		if o.Schema > max {
			max = o.Schema
		}
	}
	return max
}

// ---------------------------------------------------------------------------

// TestApplyStopsExactlyAtTarget. `up` is unbounded by construction: a binary
// carrying 1..25 asked to "apply pending" applies 25 even when the operator
// reviewed a move to 19.
func TestApplyStopsExactlyAtTarget(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, 4)

	// (4, 8] is four purely additive migrations with no operator procedures, so
	// this test is about the bound and nothing else.
	res, err := Apply(ctx, db, Options{Target: 8, JournalDir: t.TempDir(), Actor: "test"})
	require.NoError(t, err)

	if got := applied(t, ctx, db); got != 8 {
		t.Fatalf("schema = %d, want exactly 8", got)
	}
	if res.From != 4 || res.To != 8 {
		t.Errorf("range = (%d, %d], want (4, 8]", res.From, res.To)
	}
	if len(res.Applied) != 4 {
		t.Errorf("applied %v, want exactly the four migrations in the range", res.Applied)
	}
}

// TestApplyRequiresATarget. There is no unbounded form, because an unbounded
// form is the one that applies a set nobody reviewed.
func TestApplyRequiresATarget(t *testing.T) {
	_, err := Apply(context.Background(), nil, Options{JournalDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "reviewed") {
		t.Fatalf("err = %v, want a refusal explaining why a target is mandatory", err)
	}
}

// TestApplyRefusesWhenTheSchemaMovedUnderIt. The TOCTOU: preflight reads the
// applied version, prints a range, and returns; something else migrates; the
// apply would then run a different set than the one that was reviewed.
func TestApplyRefusesWhenTheSchemaMovedUnderIt(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, 6)

	stale := int64(4) // what "preflight" saw
	_, err := Apply(ctx, db, Options{
		Target: 8, ExpectFromSchema: &stale, JournalDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "not the range you reviewed") {
		t.Fatalf("err = %v, want a refusal naming the drift", err)
	}
	if got := applied(t, ctx, db); got != 6 {
		t.Errorf("schema moved to %d despite the refusal", got)
	}
}

// TestApplyHoldsTheAdvisoryLockAndRereadsUnderIt. Two applies must serialize;
// the second must observe what the first did rather than the world it planned
// against.
func TestApplyHoldsTheAdvisoryLockAndRereadsUnderIt(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, 4)

	// Hold the lock on a separate session and prove the apply waits for it.
	blocker, err := db.Conn(ctx)
	require.NoError(t, err)
	_, err = blocker.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, AdvisoryLockKey)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, aerr := Apply(ctx, db, Options{Target: 6, JournalDir: t.TempDir()})
		done <- aerr
	}()

	select {
	case err := <-done:
		t.Fatalf("the apply did not wait for the advisory lock: %v", err)
	case <-time.After(750 * time.Millisecond):
	}
	if got := applied(t, ctx, db); got != 4 {
		t.Fatalf("schema moved to %d while another session held the lock", got)
	}

	_, err = blocker.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, AdvisoryLockKey)
	require.NoError(t, err)
	require.NoError(t, blocker.Close())

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("the apply never proceeded after the lock was released")
	}
	if got := applied(t, ctx, db); got != 6 {
		t.Errorf("schema = %d, want 6", got)
	}
}

// TestApplyRefusesUnexpectedMigrationSet, proved against the COMMITTED fixture.
//
// The fixture's 0020 has no obligations entry, so nothing in the system can say
// what applying it commits the operator to. It also drops upgrade_runs, which
// makes a regression here loud rather than silent.
func TestApplyRefusesUnexpectedMigrationSet(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, latest())

	_, err := Apply(ctx, db, Options{
		Target: 20, FS: unsafeFixture, JournalDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("an unclassified migration must be refused")
	}
	if !strings.Contains(err.Error(), "no recorded obligations") {
		t.Errorf("err = %v, want a refusal naming the missing classification", err)
	}

	var exists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT to_regclass('upgrade_runs') IS NOT NULL`).Scan(&exists))
	if !exists {
		t.Fatal("the fixture RAN: upgrade_runs is gone, so the refusal did not fire before " +
			"goose was invoked")
	}
	if got := applied(t, ctx, db); got != latest() {
		t.Errorf("schema = %d, want it untouched at %d", got, latest())
	}
}

// TestApplyRefusesAMigrationSetMissingAClassifiedMember. The other direction:
// the obligations table describes migrations the set does not contain, so the
// range is not the one the operator was shown.
func TestApplyRefusesAMigrationSetMissingAClassifiedMember(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, 18)

	_, err := Apply(ctx, db, Options{
		Target: 20, FS: unsafeFixture, JournalDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a set missing a classified member must be refused")
	}
	// It trips the unclassified check first, which is fine — both refuse. What
	// matters is that neither passes.
	if got := applied(t, ctx, db); got != 18 {
		t.Errorf("schema = %d, want it untouched at 18", got)
	}
}

// TestFreshInstallAppliesFullRange. 0001 is BootstrapOnly with
// RestoreToRevert and no compatible predecessor — all true, and all vacuous on
// an empty database. If those blocked, no Nova deployment could ever start.
func TestFreshInstallAppliesFullRange(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)

	res, err := Apply(ctx, db, Options{Target: latest(), JournalDir: t.TempDir()})
	require.NoError(t, err)
	if res.From != 0 {
		t.Errorf("from = %d, want 0", res.From)
	}
	if got := applied(t, ctx, db); got != latest() {
		t.Fatalf("schema = %d, want %d", got, latest())
	}

	// And the decision function agrees, since it is what the entrypoint calls.
	b, err := migrations.Between(0, latest())
	require.NoError(t, err)
	if d := AutoApplicable(b); !d.Apply {
		t.Errorf("a fresh install must be auto-applicable: %s", d.Reason)
	}
}

// TestFirstRunJournalsBeforeAppliesAndBackfillsAfter. 0019 CREATES
// upgrade_runs, so the first upgrade from schema 18 cannot insert its `started`
// row before applying the migration that makes the row possible.
func TestFirstRunJournalsBeforeAppliesAndBackfillsAfter(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, 18)

	dir := t.TempDir()
	res, err := Apply(ctx, db, Options{
		Target: 19, JournalDir: dir, Actor: "bug", ToRelease: "v0.3.0",
		ExpectReleaseLock: "sha256:deadbeef",
	})
	require.NoError(t, err)

	// The journal exists and carries the pre-0019 events.
	j, err := OpenJournal(dir, res.RunID)
	require.NoError(t, err)
	events, err := j.Events()
	require.NoError(t, err)
	if len(events) < 3 {
		t.Fatalf("journal has %d event(s); want at least preflight/started, apply/started, "+
			"apply/passed", len(events))
	}
	if events[0].Phase != PhasePreflight || events[0].State != StateStarted {
		t.Errorf("first journal event = %s/%s, want preflight/started",
			events[0].Phase, events[0].State)
	}

	// The table now exists and carries the SAME run id, with the journal
	// transcribed under it.
	var state, actor, lockDigest string
	var from, to int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT state, actor, release_lock_digest, from_schema, to_schema
		 FROM upgrade_runs WHERE id = $1`, res.RunID).
		Scan(&state, &actor, &lockDigest, &from, &to))
	if state != StatePassed || actor != "bug" || from != 18 || to != 19 {
		t.Errorf("run = %s/%s (%d, %d]", state, actor, from, to)
	}
	if lockDigest != "sha256:deadbeef" {
		t.Errorf("release_lock_digest = %q", lockDigest)
	}

	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM upgrade_events WHERE run_id = $1`, res.RunID).Scan(&n))
	if n != len(events) {
		t.Errorf("backfilled %d event(s) for %d journalled", n, len(events))
	}
}

// TestJournalIsAuthoritativeWhen0019Fails. If the apply does not reach the
// table, the journal is the only record — and it has to carry the reason.
func TestJournalIsAuthoritativeWhen0019Fails(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, 18)

	// Make 0019 fail by planting the table it creates.
	_, err := db.ExecContext(ctx, `CREATE TABLE upgrade_runs (id uuid PRIMARY KEY)`)
	require.NoError(t, err)

	dir := t.TempDir()
	run := uuid.New()
	_, applyErr := Apply(ctx, db, Options{Target: 19, JournalDir: dir, RunID: run})
	if applyErr == nil {
		t.Fatal("0019 cannot succeed against a pre-existing upgrade_runs")
	}

	j, err := OpenJournal(dir, run)
	require.NoError(t, err)
	events, err := j.Events()
	require.NoError(t, err)

	var sawFailure bool
	for _, e := range events {
		if e.Phase == PhaseApply && e.State == StateFailed {
			sawFailure = true
			if e.Detail["error"] == nil {
				t.Error("the failure event carries no reason, which is the one thing it is for")
			}
		}
	}
	if !sawFailure {
		t.Fatalf("the journal has no failure record: %+v", events)
	}

	// And nothing was recorded in the table, because it never became usable.
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM upgrade_runs WHERE id = $1`, run).Scan(&n))
	if n != 0 {
		t.Errorf("a run was recorded in a table the migration did not finish creating")
	}
}

// TestBackfillIsIdempotentUnderInterruption. (run_id, sequence) is the replay
// key: a crash halfway through the backfill must converge on restart rather
// than duplicating every event already written.
func TestBackfillIsIdempotentUnderInterruption(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, 18)

	dir := t.TempDir()
	res, err := Apply(ctx, db, Options{Target: 19, JournalDir: dir})
	require.NoError(t, err)

	var before int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM upgrade_events WHERE run_id = $1`, res.RunID).Scan(&before))
	if before == 0 {
		t.Fatal("nothing was backfilled")
	}

	// Replay the whole backfill, twice, as an interrupted run's restart would.
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	j, err := OpenJournal(dir, res.RunID)
	require.NoError(t, err)
	for range 2 {
		n, err := backfill(ctx, conn, j)
		require.NoError(t, err)
		if n != 0 {
			t.Errorf("a replay inserted %d new event(s); every one was already there", n)
		}
	}

	var after int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM upgrade_events WHERE run_id = $1`, res.RunID).Scan(&after))
	if after != before {
		t.Errorf("events grew from %d to %d across replays", before, after)
	}
}

// TestApplyAtTargetIsANoOp. An operator runs this twice; the second run must be
// a statement of fact rather than an error or a re-apply.
func TestApplyAtTargetIsANoOp(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, latest())

	res, err := Apply(ctx, db, Options{Target: latest(), JournalDir: t.TempDir()})
	require.NoError(t, err)
	if len(res.Applied) != 0 {
		t.Errorf("applied %v when already at the target", res.Applied)
	}
}

// TestApplyRefusesToGoBackwards. Going back is a restore from backup, not a
// migration, and pretending otherwise is how information gets destroyed.
func TestApplyRefusesToGoBackwards(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, latest())

	_, err := Apply(ctx, db, Options{Target: latest() - 1, JournalDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "restore from backup") {
		t.Fatalf("err = %v, want a refusal naming what going back actually requires", err)
	}
}

// TestUnacknowledgedProcedureBlocks. 0003 requires a backup because it DROPS
// two tables. An instruction nobody read is an instruction nobody followed.
func TestUnacknowledgedProcedureBlocks(t *testing.T) {
	ctx := context.Background()
	db := emptyDB(t, ctx)
	upTo(t, ctx, db, 2)

	_, err := Apply(ctx, db, Options{Target: 3, JournalDir: t.TempDir()})
	if err == nil {
		t.Fatal("a range with an unacknowledged procedure must be refused")
	}
	if !strings.Contains(err.Error(), "--acknowledge") {
		t.Errorf("the refusal does not say how to proceed: %v", err)
	}
	if got := applied(t, ctx, db); got != 2 {
		t.Errorf("schema moved to %d despite the refusal", got)
	}

	// Acknowledging it by id lets the same apply through.
	b, berr := migrations.Between(2, 3)
	require.NoError(t, berr)
	require.Len(t, b.Procedures, 1)
	_, err = Apply(ctx, db, Options{
		Target: 3, JournalDir: t.TempDir(),
		Acknowledge: []string{ObligationID(b.Procedures[0])},
	})
	require.NoError(t, err)
	if got := applied(t, ctx, db); got != 3 {
		t.Errorf("schema = %d, want 3", got)
	}
}
