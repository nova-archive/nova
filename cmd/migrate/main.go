// Package main is the Nova migration runner. It loads the embedded
// SQL files from internal/db/migrations and applies them in order
// against the database identified by DATABASE_URL.
//
// Subcommands:
//
//	migrate apply --to <n>      apply exactly (applied, n], under an advisory lock
//	migrate auto                the entrypoint's policy: apply only if the whole
//	                            range is safe unattended, otherwise stop and say why
//	migrate up                  apply everything this binary carries, bounded by it
//	migrate status              show applied/pending
//	migrate version             show the applied schema version
//	migrate --version           show build identity
//
// `migrate down` is deliberately absent (P2-M7.3, D-M7.3-9a). The normative
// contract is restore-only rollback — see internal/db/migrations/migrations.go —
// and shipping a `down` subcommand contradicts it while inviting exactly the
// operation the contract forbids. The down blocks remain in the .sql files for
// test fixtures; the production binary does not expose them.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/nova-archive/nova/internal/buildinfo"
	"github.com/nova-archive/nova/internal/db/migrations"
	"github.com/nova-archive/nova/internal/release"
	"github.com/nova-archive/nova/internal/upgrade"
)

// applyTimeout bounds a single apply. Long enough for an index build on a
// corpus-scale table, short enough that a wedged migration does not hold the
// advisory lock forever.
const applyTimeout = 60 * time.Minute

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]

	// --version is build identity and needs no database. `migrate version`
	// keeps its existing meaning — the applied schema version — which is why
	// these are deliberately different spellings (P2-M7.3, P0-c).
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("migrate", buildinfo.String())
		fmt.Println(" ", release.Compiled().String())
		return nil
	}

	if len(args) == 0 {
		return errors.New("usage: migrate <apply|auto|up|status|version>")
	}

	// Everything that can be decided WITHOUT a database is decided first. A
	// command that is going to be refused should not require a working
	// connection to be refused, and a malformed environment variable should not
	// be reported as a connection failure.
	switch args[0] {
	case "down":
		return errors.New("`migrate down` does not exist. Nova's rollback contract is " +
			"restore-from-backup: the schema is forward-only, and a down migration would " +
			"destroy information the forward one committed to. Run `novactl upgrade status` " +
			"to see what going back from here actually requires")
	case "apply", "auto", "up", "status", "version":
	default:
		return fmt.Errorf("unknown subcommand: %s", args[0])
	}

	// The entrypoint's switch, parsed before anything else so a typo is an
	// error rather than a silent "false".
	migrateOnStart, err := upgrade.ParseBool("NOVA_MIGRATE_ON_START",
		os.Getenv("NOVA_MIGRATE_ON_START"), true)
	if err != nil {
		return err
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	goose.SetBaseFS(migrations.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	switch args[0] {
	case "apply":
		return cmdApply(ctx, db, args[1:])
	case "auto":
		return cmdAuto(ctx, db, migrateOnStart, args[1:])
	case "up":
		// Bounded by what this binary carries, and routed through the same
		// locked, journalled, obligation-checked path as `apply`. The
		// convenience stays; the unreviewed-set hazard does not.
		return cmdApply(ctx, db, append([]string{"--to", fmt.Sprint(latestSchema())}, args[1:]...))
	case "status":
		return goose.StatusContext(ctx, db, ".")
	case "version":
		return goose.VersionContext(ctx, db, ".")
	}
	// The subcommand was validated before the database was opened, so there is
	// no default arm here: an unknown one never reaches this switch.
	return fmt.Errorf("unreachable: %s", args[0])
}

// latestSchema is the highest version this binary carries.
func latestSchema() int64 {
	var max int64
	for _, o := range migrations.All() {
		if o.Schema > max {
			max = o.Schema
		}
	}
	return max
}

// ---------------------------------------------------------------------------

func cmdApply(ctx context.Context, db *sql.DB, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	to := fs.Int64("to", 0, "schema version to stop at (required)")
	expectFrom := fs.Int64("expect-from", -1,
		"the applied schema preflight observed; refuses if it has moved since")
	lockDigest := fs.String("expect-release-lock", "",
		"the verified release-lock digest this apply belongs to (recorded on the run)")
	toRelease := fs.String("to-release", release.Compiled().Version, "the release being applied")
	ack := fs.String("acknowledge", "",
		"comma-separated obligation ids you have carried out")
	journalDir := fs.String("journal-dir", os.Getenv(upgrade.JournalDirEnv),
		"where the bootstrap journal is written")
	configPath := fs.String("config", defaultConfigPath(),
		"operator.yaml to fingerprint, so the run records what the configuration was going in")
	actor := fs.String("actor", os.Getenv("USER"), "who is running this")
	runID := fs.String("run-id", "", "continue an interrupted run under its existing id")
	asJSON := fs.Bool("json", false, "machine-readable result")
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := upgrade.Options{
		Target:                  *to,
		ConfigFingerprintBefore: configFingerprint(*configPath),
		ExpectReleaseLock:       *lockDigest,
		ToRelease:               *toRelease,
		Acknowledge:             splitComma(*ack),
		Actor:                   *actor,
		JournalDir:              *journalDir,
	}
	if *expectFrom >= 0 {
		opts.ExpectFromSchema = expectFrom
	}
	if *runID != "" {
		id, err := uuid.Parse(*runID)
		if err != nil {
			return fmt.Errorf("--run-id: %w", err)
		}
		opts.RunID = id
	}

	res, applyErr := upgrade.Apply(ctx, db, opts)
	if *asJSON {
		out := map[string]any{
			"run_id": res.RunID.String(), "from": res.From, "to": res.To,
			"applied": res.Applied, "journal": res.JournalPath,
			"backfilled_events": res.BackfilledEvents,
		}
		if applyErr != nil {
			out["error"] = applyErr.Error()
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
		return applyErr
	}
	if applyErr != nil {
		// The journal path goes on the failure, not the success. A successful
		// apply does not send anyone looking for a file.
		//
		// And the summary is printed HERE rather than leaving the operator to
		// read JSONL: the moment they need the record is the moment the
		// migration just failed, and telling them a path is one step short of
		// telling them what happened.
		if res.JournalPath != "" {
			fmt.Fprintf(os.Stderr, "migrate: the run is recorded at %s\n", res.JournalPath)
			if j, jerr := upgrade.OpenJournal(filepath.Dir(res.JournalPath), res.RunID); jerr == nil {
				if events, eerr := j.Events(); eerr == nil {
					fmt.Fprintln(os.Stderr, "migrate: what the run recorded:")
					upgrade.WriteSummary(os.Stderr, events)
				}
			}
		}
		return applyErr
	}

	if len(res.Applied) == 0 {
		fmt.Printf("schema %d: already at the target\n", res.From)
		return nil
	}
	fmt.Printf("applied (%d, %d]: %s\n", res.From, res.To, strings.Join(res.Applied, ", "))
	fmt.Printf("run %s, journal %s\n", res.RunID, res.JournalPath)
	if res.BackfilledEvents > 0 {
		fmt.Printf("backfilled %d journal event(s) into upgrade_events\n", res.BackfilledEvents)
	}
	return nil
}

// cmdAuto is what the container entrypoint runs.
//
// It applies only when the whole range is safe unattended, and otherwise exits
// zero having printed the command an operator must run. Exiting non-zero would
// crash-loop the coordinator over a schema decision that is waiting on a human,
// which turns "your upgrade needs attention" into "your archive is down".
func cmdAuto(ctx context.Context, db *sql.DB, enabled bool, args []string) error {
	fs := flag.NewFlagSet("auto", flag.ContinueOnError)
	journalDir := fs.String("journal-dir", os.Getenv(upgrade.JournalDirEnv),
		"where the bootstrap journal is written")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !enabled {
		fmt.Println("migrate: NOVA_MIGRATE_ON_START is off; not touching the schema.")
		fmt.Println("migrate: the coordinator will refuse to start against a stale schema, " +
			"which is the point of turning this off.")
		return nil
	}

	applied, err := appliedSchema(ctx, db)
	if err != nil {
		return err
	}
	target := latestSchema()

	boundary, err := migrations.Between(applied, target)
	if err != nil {
		return err
	}
	decision := upgrade.AutoApplicable(boundary)
	if !decision.Apply {
		fmt.Printf("migrate: %s\n", decision.Reason)
		if decision.Command != "" {
			fmt.Printf("migrate: run this yourself, from the target nova-admin:\n    %s\n",
				decision.Command)
		}
		return nil
	}

	fmt.Printf("migrate: %s\n", decision.Reason)
	return cmdApply(ctx, db, []string{
		"--to", fmt.Sprint(target),
		"--journal-dir", *journalDir,
		"--actor", "entrypoint",
	})
}

func defaultConfigPath() string {
	if p := os.Getenv("NOVA_CONFIG_FILE"); p != "" {
		return p
	}
	return "/etc/nova/operator.yaml"
}

// configFingerprint records what the configuration was going into the upgrade
// (P2-M7.3, D-M7.3-10).
//
// A missing or unreadable file is NOT fatal. The fingerprint is a record, and
// refusing to migrate because a record could not be taken would trade the
// operator's upgrade for a note in a table. It reports the reason in the
// fingerprint's place instead, so the column distinguishes "unchanged" from
// "never measured" — which an empty string could not.
func configFingerprint(path string) string {
	if path == "" {
		return "unavailable: no operator.yaml path"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: note: not fingerprinting the configuration (%v)\n", err)
		return "unavailable: " + err.Error()
	}
	fp, findings, err := release.Fingerprint(b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: note: %v\n", err)
		return "unavailable: " + err.Error()
	}
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "migrate: %s: %s\n", f.Path, f.Reason)
	}
	return fp
}

func appliedSchema(ctx context.Context, db *sql.DB) (int64, error) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v); err != nil {
		return 0, nil // no goose table: a fresh install, not a failure
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

func splitComma(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Make stdlib's pgx driver registration explicit so static checkers don't
// trim the side-effect import.
var _ = stdlib.GetDefaultDriver
