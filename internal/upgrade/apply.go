package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/nova-archive/nova/internal/db/migrations"
)

// One advisory-locked, target-bounded apply (P2-M7.3, D-M7.3-9a).
//
// # Why not `plan` followed by `up`
//
// Because they can disagree. `plan` reads the applied version, prints a range,
// and returns; `up` then reads it again and applies whatever is pending. Between
// the two, another process can move the schema, and the operator ends up
// applying a set nobody reviewed. So there is one command: it takes the
// advisory lock, RE-READS the applied version under the lock, computes
// `(applied, target]` there, refuses anything that is not what the operator was
// told, and stops exactly at the target.
//
// # Why a target at all
//
// `up` is unbounded by construction. A binary that carries migrations 1..25 and
// is asked to "apply pending" will apply 25 even when the operator reviewed a
// move to 19. The target is the reviewed decision, and it is not optional.

// AdvisoryLockKey is the pg_advisory_lock key every migration apply contends
// on. A fixed constant rather than a hash of anything: two binaries from
// different releases must serialize against each other, so the key cannot be
// derived from something that varies between them.
const AdvisoryLockKey int64 = 7320251118

// SchemaWithUpgradeRuns is the migration that creates upgrade_runs. Below it,
// the journal is the only place a run can be recorded.
const SchemaWithUpgradeRuns int64 = 19

// Options is one apply.
type Options struct {
	// Target is the schema version to stop at. Required: an unbounded apply
	// can apply a set the operator never reviewed.
	Target int64

	// ExpectFromSchema, when non-nil, is the applied version preflight
	// observed. Under the lock, a different value means something moved in
	// between and this apply is not the one that was reviewed.
	ExpectFromSchema *int64

	// ExpectReleaseLock is the verified release-lock digest this apply belongs
	// to. Recorded, not enforced: the lock is verified by the bootstrap, and
	// re-deriving trust here would be a second, weaker check.
	ExpectReleaseLock string
	// ToRelease and FromRelease name the versions for the run record.
	ToRelease, FromRelease string

	// Acknowledge lists obligation ids the operator has accepted. An obligation
	// the range carries and this does not name blocks the apply.
	Acknowledge []string

	// Actor is who ran it.
	Actor string

	// JournalDir is where the bootstrap journal is written.
	JournalDir string

	// RunID continues an interrupted run. Zero means a new one.
	RunID uuid.UUID

	// FS overrides the embedded migration set. Tests use it to drive a
	// committed fixture; production leaves it nil.
	FS fs.FS

	// ConfigFingerprintBefore records the effective configuration going in.
	ConfigFingerprintBefore string
}

// Result is what happened.
type Result struct {
	RunID       uuid.UUID
	From, To    int64
	Applied     []string
	Boundary    migrations.Boundary
	JournalPath string
	// BackfilledEvents counts journal events transcribed into upgrade_events.
	BackfilledEvents int
}

// Apply performs the whole sequence: journal, lock, re-read, refuse, apply,
// backfill, complete.
//
// db is used for goose and for the run record alike. One connection pool means
// the advisory lock and the writes it protects cannot end up on different
// sessions, which is the failure mode a "just use another pool" convenience
// would introduce.
func Apply(ctx context.Context, db *sql.DB, opts Options) (Result, error) {
	if opts.Target <= 0 {
		return Result{}, errors.New("upgrade: --to is required. An unbounded apply can apply a " +
			"set nobody reviewed; the target IS the reviewed decision")
	}
	if opts.JournalDir == "" {
		opts.JournalDir = DefaultJournalDir
	}
	if opts.RunID == uuid.Nil {
		opts.RunID = uuid.New()
	}

	j, err := OpenJournal(opts.JournalDir, opts.RunID)
	if err != nil {
		return Result{}, err
	}
	res := Result{RunID: opts.RunID, To: opts.Target, JournalPath: j.Path()}

	if _, err := j.Record(PhasePreflight, StateStarted, map[string]any{
		"target": opts.Target, "actor": opts.Actor, "release_lock": opts.ExpectReleaseLock,
	}); err != nil {
		return res, err
	}

	// --- the lock, and everything decided under it --------------------------
	conn, err := db.Conn(ctx)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, AdvisoryLockKey); err != nil {
		return res, fmt.Errorf("upgrade: could not take the migration advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, AdvisoryLockKey)
	}()

	applied, err := appliedSchema(ctx, conn)
	if err != nil {
		return res, err
	}
	res.From = applied

	if opts.ExpectFromSchema != nil && *opts.ExpectFromSchema != applied {
		return res, journalFail(j, fmt.Errorf(
			"upgrade: preflight saw schema %d but the database is at %d under the lock. "+
				"Something else migrated in between, so this is not the range you reviewed; "+
				"re-run preflight", *opts.ExpectFromSchema, applied))
	}
	if applied == opts.Target {
		if _, err := j.Record(PhaseApply, StateSkipped, map[string]any{
			"schema": applied, "reason": "already at the target",
		}); err != nil {
			return res, err
		}
		res.Boundary = migrations.Boundary{From: applied, To: applied,
			OldBinaryCompatible: true, OnlineApplicable: true}
		return res, nil
	}
	if applied > opts.Target {
		return res, journalFail(j, fmt.Errorf(
			"upgrade: the database is at schema %d, ahead of the target %d. The schema is "+
				"forward-only; going back is a restore from backup, not a migration",
			applied, opts.Target))
	}

	boundary, err := migrations.Between(applied, opts.Target)
	if err != nil {
		return res, journalFail(j, err)
	}
	res.Boundary = boundary

	fsys := opts.FS
	if fsys == nil {
		fsys = migrations.Migrations
	}
	pending, err := pendingFiles(fsys, applied, opts.Target)
	if err != nil {
		return res, journalFail(j, err)
	}
	if err := assertExpectedSet(pending, applied, opts.Target); err != nil {
		return res, journalFail(j, err)
	}
	if err := assertAcknowledged(boundary, opts.Acknowledge); err != nil {
		return res, journalFail(j, err)
	}
	res.Applied = pending

	// --- open the run record where it is possible to ------------------------
	//
	// At or above 19 the table already exists, so the run is opened BEFORE
	// anything is applied and an interruption leaves a `started` row. Below it,
	// the journal is the record until 0019 lands and the backfill catches up.
	runOpen := false
	if applied >= SchemaWithUpgradeRuns {
		if err := openRun(ctx, conn, opts, applied); err != nil {
			return res, journalFail(j, err)
		}
		runOpen = true
	}

	if _, err := j.Record(PhaseApply, StateStarted, map[string]any{
		"from": applied, "to": opts.Target, "files": pending,
		"old_binary_compatible": boundary.OldBinaryCompatible,
		"restore_to_revert":     boundary.RestoreToRevert,
	}); err != nil {
		return res, err
	}

	// --- apply, bounded -----------------------------------------------------
	goose.SetBaseFS(fsys)
	if err := goose.SetDialect("postgres"); err != nil {
		return res, journalFail(j, err)
	}
	if err := goose.UpToContext(ctx, db, ".", opts.Target); err != nil {
		if runOpen {
			_ = completeRun(ctx, conn, opts.RunID, StateFailed, "")
		}
		return res, journalFail(j, fmt.Errorf("upgrade: applying (%d, %d]: %w",
			applied, opts.Target, err))
	}

	if _, err := j.Record(PhaseApply, StatePassed, map[string]any{
		"from": applied, "to": opts.Target,
	}); err != nil {
		return res, err
	}

	// --- backfill ------------------------------------------------------------
	//
	// The table may only just have come into existence. Everything already in
	// the journal is transcribed under the SAME run id, and (run_id, sequence)
	// makes a replay after an interrupted backfill converge instead of
	// duplicating.
	if opts.Target >= SchemaWithUpgradeRuns {
		if !runOpen {
			if err := openRun(ctx, conn, opts, applied); err != nil {
				return res, journalFail(j, err)
			}
		}
		n, err := backfill(ctx, conn, j)
		if err != nil {
			return res, journalFail(j, err)
		}
		res.BackfilledEvents = n
		if err := completeRun(ctx, conn, opts.RunID, StatePassed, ""); err != nil {
			return res, journalFail(j, err)
		}
	}
	return res, nil
}

// journalFail records the failure before returning it, so the reason survives
// the process that discovered it.
func journalFail(j *Journal, cause error) error {
	_, _ = j.Record(PhaseApply, StateFailed, map[string]any{"error": cause.Error()})
	return cause
}

func appliedSchema(ctx context.Context, conn *sql.Conn) (int64, error) {
	var v sql.NullInt64
	err := conn.QueryRowContext(ctx,
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v)
	if err != nil {
		// No goose table: nothing has been applied here, which is a fresh
		// install rather than a failure.
		return 0, nil
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// pendingFiles lists the migrations in (applied, target] as they exist in the
// migration set actually being applied.
func pendingFiles(fsys fs.FS, applied, target int64) ([]string, error) {
	entries, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := path.Base(e)
		n, err := schemaNumber(name)
		if err != nil {
			return nil, fmt.Errorf("upgrade: %s does not start with a version number, so its "+
				"place in the sequence cannot be established: %w", name, err)
		}
		if n > applied && n <= target {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func schemaNumber(name string) (int64, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, errors.New("no NNNN_ prefix")
	}
	return strconv.ParseInt(prefix, 10, 64)
}

// assertExpectedSet refuses a migration set the binary cannot account for.
//
// Two ways it goes wrong. A file in the range with no obligations entry is a
// migration whose MEANING nothing states — it cannot be classified, so it
// cannot be auto-applied, planned around or rolled back knowingly. And a
// classified migration missing from the range is a set that does not match the
// one the obligations table describes. Either way the range the operator was
// shown is not the range about to run.
func assertExpectedSet(pending []string, applied, target int64) error {
	var unclassified []string
	for _, f := range pending {
		if _, err := migrations.Lookup(f); err != nil {
			unclassified = append(unclassified, f)
		}
	}
	if len(unclassified) > 0 {
		return fmt.Errorf("upgrade: %s in (%d, %d] %s no recorded obligations. A migration "+
			"whose meaning nothing states cannot be applied by a command that reports what "+
			"applying it commits you to; add it to internal/db/migrations/obligations.go",
			strings.Join(unclassified, ", "), applied, target,
			map[bool]string{true: "has", false: "have"}[len(unclassified) == 1])
	}

	var missing []string
	for _, o := range migrations.All() {
		if o.Schema > applied && o.Schema <= target && !slices.Contains(pending, o.File) {
			missing = append(missing, o.File)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("upgrade: the obligations table describes %s in (%d, %d] but the "+
			"migration set does not contain %s. This is not the range you reviewed",
			strings.Join(missing, ", "), applied, target,
			map[bool]string{true: "it", false: "them"}[len(missing) == 1])
	}
	return nil
}

// assertAcknowledged blocks on an obligation the operator has not accepted.
//
// Procedures are the acknowledgeable unit because they are the ones that
// require the operator to DO something. The composed flags are reported by
// preflight and printed here, but a flag is a property of the range; a
// procedure is an instruction, and an unfollowed instruction is what breaks an
// upgrade.
func assertAcknowledged(b migrations.Boundary, acknowledged []string) error {
	if b.BootstrapOnly && b.From == 0 {
		// A fresh install. 0003's procedure is "back up the database, this
		// DROPS integrity_audits and audit_log" — true, and vacuous against an
		// empty one. Requiring an acknowledgement here would mean no Nova
		// deployment could ever start without an operator typing a flag to
		// promise they backed up nothing.
		return nil
	}

	var unmet []string
	for _, p := range b.Procedures {
		if !slices.Contains(acknowledged, ObligationID(p)) {
			unmet = append(unmet, fmt.Sprintf("%s\n      %s", ObligationID(p), p))
		}
	}
	if len(unmet) == 0 {
		return nil
	}
	return fmt.Errorf("upgrade: this range requires %d operator procedure(s) you have not "+
		"acknowledged. Do them, then re-run with --acknowledge <id> for each:\n    - %s",
		len(unmet), strings.Join(unmet, "\n    - "))
}

// ObligationID is the stable, typeable handle for a procedure.
//
// The procedure text is a sentence. An operator cannot be asked to retype a
// sentence exactly, and a positional index would silently re-point at a
// different procedure when the range changes — acknowledging "the second one"
// is how the wrong thing gets waived.
//
// A truncated slug alone is not enough. Two of the real procedures begin
// "Expect a backfill proportional to the blob count", so a prefix would give
// them the SAME id and acknowledging one would silently acknowledge the other.
// The digest suffix is what makes the handle refer to exactly one sentence.
func ObligationID(procedure string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(procedure) {
		if b.Len() >= 32 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && b.String()[b.Len()-1] != '-':
			b.WriteByte('-')
		}
	}
	sum := sha256.Sum256([]byte(procedure))
	return strings.Trim(b.String(), "-") + "-" + hex.EncodeToString(sum[:3])
}

// ---------------------------------------------------------------------------
// The run record
// ---------------------------------------------------------------------------

func openRun(ctx context.Context, conn *sql.Conn, opts Options, from int64) error {
	obligations, err := json.Marshal(map[string]any{
		"acknowledged": opts.Acknowledge,
	})
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `
		INSERT INTO upgrade_runs (
		    id, from_release, to_release, from_schema, to_schema, release_lock_digest,
		    obligations, config_fingerprint_before, state, actor
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'started', $9)
		ON CONFLICT (id) DO NOTHING`,
		opts.RunID, opts.FromRelease, opts.ToRelease, from, opts.Target,
		opts.ExpectReleaseLock, obligations, opts.ConfigFingerprintBefore, opts.Actor)
	return err
}

func completeRun(ctx context.Context, conn *sql.Conn, run uuid.UUID, state, fingerprintAfter string) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE upgrade_runs
		SET state = $2, config_fingerprint_after = $3, completed_at = now()
		WHERE id = $1`, run, state, fingerprintAfter)
	return err
}

// backfill transcribes the journal into upgrade_events.
//
// Idempotent by (run_id, sequence): a crash halfway through would otherwise
// duplicate every event already written when the operator restarts, and an
// upgrade record that grows on each retry is worse than none.
func backfill(ctx context.Context, conn *sql.Conn, j *Journal) (int, error) {
	events, err := j.Events()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range events {
		detail, err := json.Marshal(e.Detail)
		if err != nil {
			return n, err
		}
		if e.Detail == nil {
			detail = []byte(`{}`)
		}
		res, err := conn.ExecContext(ctx, `
			INSERT INTO upgrade_events (run_id, sequence, phase, state, detail, recorded_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (run_id, sequence) DO NOTHING`,
			e.RunID, e.Sequence, e.Phase, e.State, detail, e.At)
		if err != nil {
			return n, err
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			n++
		}
	}
	return n, nil
}
