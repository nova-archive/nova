package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/migrations"
	"github.com/nova-archive/nova/internal/release"
)

// `novactl upgrade check` — preflight against a VERIFIED target lock
// (P2-M7.3, D-M7.3-5, D-M7.3-11, D-M7.3-13, D-M7.3-20).
//
// # Requirement and outcome are separate axes
//
// A single enum in which "disk headroom could not be checked" reads the same as
// "disk headroom is fine" silently permits a dangerous migration. So every
// check reports a REQUIREMENT (required/optional) and an OUTCOME
// (passed/failed/skipped), and a REQUIRED SKIPPED check blocks unless the
// operator acknowledges it by name.
//
// # It mutates nothing and fetches nothing
//
// Preflight that changes state cannot be run twice, and an operator will run
// this twice. T1.22 forbids fetching, so the target's identity comes from its
// compiled-in catalog and the target's lock comes from a file the operator
// verified out of band.
//
// # What blocks, and what does not
//
// Genuine incompatibility blocks: no common protocol, a missing mandatory
// capability, unresolved drain debt. A version STRING never blocks — it is an
// untrusted self-report, and the interop contract is protocol plus
// capabilities.

const (
	required = "required"
	optional = "optional"

	passed  = "passed"
	failed  = "failed"
	skipped = "skipped"
)

type checkResult struct {
	ID          string `json:"id"`
	Requirement string `json:"requirement"`
	Outcome     string `json:"outcome"`
	Detail      string `json:"detail"`
}

// blocks reports whether this result stops the upgrade.
//
// A required check that could not run blocks: not knowing is not the same as
// being fine, and the acknowledgement is the operator saying so explicitly.
//
// Acknowledgement waives a SKIPPED check and nothing else. It means "I checked
// this myself" — a claim an operator can make about disk headroom, and cannot
// make about a donor that just told the coordinator it speaks no compatible
// protocol. A failure is a measured fact, and there is no flag for disbelieving
// one.
func (c checkResult) blocks(acknowledged []string) bool {
	if c.Outcome == failed {
		return true
	}
	if c.Outcome != skipped || c.Requirement != required {
		return false
	}
	return !slices.Contains(acknowledged, c.ID)
}

type checkReport struct {
	Target struct {
		Version      string `json:"version"`
		LockDigest   string `json:"lock_digest"`
		TargetSchema int    `json:"target_schema"`
	} `json:"target"`
	RollbackBoundary string        `json:"rollback_boundary"`
	Checks           []checkResult `json:"checks"`
	Blocked          bool          `json:"blocked"`
}

func cmdUpgradeCheck(args []string) error {
	fs := flag.NewFlagSet("upgrade check", flag.ContinueOnError)
	lockPath := fs.String("lock", "", "release lock file from the verified bundle")
	intentPath := fs.String("intent", "", "release intent file from the same bundle")
	expectDigest := fs.String("expect-lock-digest", "",
		"the lock digest you verified out of band (sha256:...)")
	operatorYAML := fs.String("operator-yaml", "/etc/nova/operator.yaml", "operator.yaml to inspect")
	ack := fs.String("acknowledge", "",
		"comma-separated check ids you have verified yourself (a required check that could not "+
			"run blocks until you say so by name; a check that FAILED is not waivable)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	for name, v := range map[string]string{
		"--lock": *lockPath, "--intent": *intentPath, "--expect-lock-digest": *expectDigest,
	} {
		if v == "" {
			return fmt.Errorf("%s is required; preflight evaluates a target you have already "+
				"verified, not one it fetches", name)
		}
	}

	lock, err := loadVerifiedLock(*lockPath, *intentPath, *expectDigest)
	if err != nil {
		return err
	}

	var acknowledged []string
	if *ack != "" {
		acknowledged = splitComma(*ack)
	}

	ctx := context.Background()
	pool := optionalPool(ctx)
	if pool != nil {
		defer pool.Close()
	}

	rep := runPreflight(ctx, pool, lock, *expectDigest, *operatorYAML, acknowledged)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		printCheck(rep)
	}
	if rep.Blocked {
		return errors.New("preflight blocked; resolve the failures above or acknowledge the " +
			"skipped required checks by id")
	}
	return nil
}

func runPreflight(ctx context.Context, pool *pgxpool.Pool, lock release.Lock,
	lockDigest, operatorYAML string, acknowledged []string,
) checkReport {
	cat := release.Compiled()

	var rep checkReport
	rep.Target.Version = lock.Version
	rep.Target.LockDigest = lockDigest
	rep.Target.TargetSchema = lock.TargetSchema

	add := func(id, req, outcome, detail string) {
		rep.Checks = append(rep.Checks, checkResult{ID: id, Requirement: req, Outcome: outcome, Detail: detail})
	}

	// 1. THE ROLLBACK BOUNDARY, FIRST. It is the one answer an operator needs
	//    before starting rather than after failing.
	applied := int64(-1)
	if pool != nil {
		if v, err := release.AppliedSchema(ctx, pool); err == nil {
			applied = v
		}
	}
	switch {
	case applied < 0:
		rep.RollbackBoundary = "unknown — the database is unreachable, so the migration range " +
			"cannot be computed"
		add("rollback-boundary", required, skipped, rep.RollbackBoundary)
	default:
		b, err := migrations.Between(applied, int64(lock.TargetSchema))
		if err != nil {
			rep.RollbackBoundary = err.Error()
			add("rollback-boundary", required, failed, err.Error())
		} else {
			rep.RollbackBoundary = rollbackBoundary(b)
			add("rollback-boundary", required, passed, rep.RollbackBoundary)

			// 2. The migration path itself, with its obligations.
			detail := fmt.Sprintf("(%d, %d]: online=%t maintenance=%t restore-to-revert=%t",
				b.From, b.To, b.OnlineApplicable, b.RequiresMaintenance, b.RestoreToRevert)
			for _, p := range b.Procedures {
				detail += "\n      procedure: " + p
			}
			add("migration-path", required, passed, detail)
		}
	}

	// 3. Version skip: this release's catalog must name the running one, or the
	//    operator is crossing a gap nothing has tested.
	if cat.Stamped() && applied >= 0 {
		if applied > int64(cat.TargetSchema) {
			add("version-skip", required, failed, fmt.Sprintf(
				"the database is at schema %d, ahead of this release's %d; the schema is "+
					"forward-only and this binary is older than the deployment", applied, cat.TargetSchema))
		} else {
			add("version-skip", required, passed, fmt.Sprintf(
				"schema %d → %d, within this release's declared range", applied, cat.TargetSchema))
		}
	} else {
		add("version-skip", required, skipped, "no applied schema to compare against")
	}

	// 4. Config keys — WARN ONLY. Turning a months-old harmless misspelling into
	//    a hard failure during an upgrade would be the wrong moment to start.
	if b, err := os.ReadFile(operatorYAML); err != nil {
		add("config-keys", optional, skipped, fmt.Sprintf("%s: %v", operatorYAML, err))
	} else if findings, err := release.InspectConfigKeys(b); err != nil {
		add("config-keys", required, failed, err.Error())
	} else if len(findings) == 0 {
		add("config-keys", optional, passed, "every key is recognized")
	} else {
		detail := fmt.Sprintf("%d unrecognized key(s), IGNORED and preserved:", len(findings))
		for _, f := range findings {
			detail += "\n      " + f.Path
		}
		add("config-keys", optional, passed, detail)
	}

	// 5. Required secrets.
	missing := []string{}
	for _, env := range []string{"DATABASE_URL"} {
		if os.Getenv(env) == "" {
			missing = append(missing, env)
		}
	}
	if len(missing) > 0 {
		add("required-secrets", required, failed, fmt.Sprintf("not set: %v", missing))
	} else {
		add("required-secrets", required, passed, "present")
	}

	// 6. Disk headroom. There is no host adapter inside a container without a
	//    Docker socket, so this is REQUIRED-SKIPPED rather than silently passed:
	//    "could not check" must not read as "fine".
	add("disk-headroom", required, skipped,
		"this runs inside nova-admin, which has no view of the host filesystem. Check free "+
			"space where the Postgres volume lives, then acknowledge with "+
			"--acknowledge disk-headroom")

	// 7. Fleet compatibility — against protocol and capabilities, never a
	//    version string.
	switch {
	case pool == nil:
		add("fleet-compatibility", required, skipped, "the database is unreachable")
	default:
		outcome, detail := fleetOutcome(ctx, pool, cat)
		add("fleet-compatibility", required, outcome, detail)
	}

	for _, c := range rep.Checks {
		if c.blocks(acknowledged) {
			rep.Blocked = true
		}
	}
	return rep
}

// fleetOutcome classifies the fleet. It returns (outcome, detail).
func fleetOutcome(ctx context.Context, pool *pgxpool.Pool, cat release.Catalog) (string, string) {
	rows, err := release.ReadCensus(ctx, pool)
	if err != nil {
		return skipped, err.Error()
	}
	now := time.Now()

	var incompatible, nonconforming, unsupported int
	for _, r := range rows {
		a := release.Classify(cat, r, now, time.Hour)
		if a.Protocol == release.Incompatible {
			incompatible++
		}
		if a.Profile == release.Nonconforming {
			nonconforming++
		}
		if a.Version == release.VersionUnsupported {
			unsupported++
		}
	}

	// Drain debt is the other genuine blocker: replacing an unsupported
	// donor's replica must finish before it goes.
	var debt int
	_ = pool.QueryRow(ctx, `
		SELECT count(*) FROM pin_assignments pa
		JOIN nodes n ON n.id = pa.node_id
		WHERE n.draining_at IS NOT NULL AND pa.state = 'acked'`).Scan(&debt)

	detail := fmt.Sprintf("%d donor(s): %d protocol-incompatible, %d outside the core profile, "+
		"%d unsupported-but-compatible, %d acked replica(s) still on draining nodes",
		len(rows), incompatible, nonconforming, unsupported, debt)

	// UNSUPPORTED DOES NOT BLOCK. Untested is not dead, and blocking on it would
	// make the support window a weapon rather than a statement.
	if incompatible > 0 {
		return failed, detail + "\n      a donor with no compatible protocol must be drained and " +
			"replaced WHILE THE OLD PROTOCOL STILL EXISTS"
	}
	if debt > 0 {
		return failed, detail + "\n      finish the drain before upgrading: those replicas have " +
			"not been re-homed yet"
	}
	return passed, detail
}

func printCheck(rep checkReport) {
	// The boundary prints FIRST, before any check result. It is what an
	// operator needs in order to decide, and burying it under a list of passes
	// is how it gets read afterwards instead.
	fmt.Printf("going back from %s: %s\n\n", rep.Target.Version, rep.RollbackBoundary)
	fmt.Printf("target %s (lock %s, schema %d)\n\n",
		rep.Target.Version, rep.Target.LockDigest, rep.Target.TargetSchema)

	for _, c := range rep.Checks {
		mark := "ok  "
		switch c.Outcome {
		case failed:
			mark = "FAIL"
		case skipped:
			mark = "skip"
		}
		fmt.Printf("%s  [%s] %-20s %s\n", mark, c.Requirement, c.ID, c.Detail)
	}
	fmt.Println()
	if rep.Blocked {
		fmt.Println("BLOCKED — a required check failed or could not run.")
		return
	}
	fmt.Println("preflight clear.")
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if v := s[start:i]; v != "" {
				out = append(out, v)
			}
			start = i + 1
		}
	}
	return out
}
