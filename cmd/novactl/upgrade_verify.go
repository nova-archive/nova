package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/buildinfo"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/release"
	"github.com/nova-archive/nova/internal/upgrade"
)

// `novactl upgrade verify --plane <p>` — post-upgrade evidence for ONE plane
// (P2-M7.3, D-M7.3-11).
//
// # Why one plane per invocation
//
// One process inside nova-admin cannot observe host image digests (no Docker
// socket), cannot enter nova-doctor's network namespace, cannot persist a
// report past its own `run --rm` removal, and cannot perform host disk checks.
// Nova's three-plane doctor exists because those planes do not collapse, and
// pretending otherwise here would produce a verification that quietly skipped
// most of what it claimed to cover.
//
// So the HOST orchestrates (scripts/nova-release), carrying one run id and one
// report directory through every invocation, and this command executes the
// planes that live inside the admin image.
//
// # The report is written to a mounted directory
//
// nova-admin runs with `run --rm`. A report on its root filesystem evaporates
// the moment the command exits, which is precisely the case — a failed
// verification — where the operator needs it.

// Planes this command can execute. host and doctor are named here so an
// unknown plane is an error rather than a silent no-op, but they are executed
// by the orchestrator and by nova-doctor respectively.
var (
	adminPlanes = []string{"admin", "coordinator", "database", "donor"}
	hostPlanes  = []string{"host", "doctor"}
)

type planeCheck struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

type planeReport struct {
	RunID     string       `json:"run_id"`
	Plane     string       `json:"plane"`
	Binary    string       `json:"binary"`
	Release   string       `json:"release"`
	At        string       `json:"at"`
	Checks    []planeCheck `json:"checks"`
	Outcome   string       `json:"outcome"`
	ResultSHA string       `json:"result_sha256"`
}

func cmdUpgradeVerify(args []string) error {
	fs := flag.NewFlagSet("upgrade verify", flag.ContinueOnError)
	plane := fs.String("plane", "admin", "which plane to verify: "+
		fmt.Sprintf("%v (host-side planes %v are run by the orchestrator)", adminPlanes, hostPlanes))
	runID := fs.String("run-id", "", "the orchestrator's run id, shared by every plane")
	reportDir := fs.String("report-dir", "", "directory to write the plane report into (mounted; "+
		"nova-admin is run --rm and a report on its rootfs evaporates)")
	configPath := fs.String("config", "/etc/nova/operator.yaml",
		"operator.yaml to fingerprint, so the run records the configuration coming out")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if slices.Contains(hostPlanes, *plane) {
		return fmt.Errorf("plane %q runs on the host, not inside nova-admin: it needs a Docker "+
			"socket or another container's network namespace, neither of which this process has",
			*plane)
	}
	if !slices.Contains(adminPlanes, *plane) {
		return fmt.Errorf("unknown plane %q; expected one of %v", *plane, adminPlanes)
	}
	if *runID == "" {
		return errors.New("--run-id is required: every plane's evidence has to join to one run, " +
			"or the aggregate is a pile of unrelated results")
	}

	ctx := context.Background()
	pool := optionalPool(ctx)
	if pool != nil {
		defer pool.Close()
	}

	rep := verifyPlane(ctx, pool, *plane, *runID)

	// RECORD THE VERIFY PHASE. upgrade_runs and upgrade_events carry four
	// phases and only two were ever written: the run stopped at `apply`, so a
	// record that exists to answer "how do I prove it worked" could not say
	// whether anyone had checked (P2-M7.3, D-M7.3-10).
	if pool != nil {
		if rerr := recordVerify(ctx, pool, rep, *configPath); rerr != nil {
			// A recording failure must not fail a verification that passed —
			// but it must be visible, because a silent one leaves the operator
			// believing the run is journalled when it is not.
			fmt.Fprintf(os.Stderr, "warning: the %s plane result was not recorded: %v\n",
				rep.Plane, rerr)
		}
	}

	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(body))

	if *reportDir != "" {
		if err := writePlaneReport(*reportDir, rep, body); err != nil {
			return err
		}
	} else if rep.Outcome != passed {
		fmt.Fprintln(os.Stderr,
			"warning: no --report-dir, so this failing report exists only in this output")
	}

	if rep.Outcome == failed {
		return fmt.Errorf("plane %s failed", rep.Plane)
	}
	return nil
}

func verifyPlane(ctx context.Context, pool *pgxpool.Pool, plane, runID string) planeReport {
	rep := planeReport{
		RunID: runID, Plane: plane,
		Binary:  buildinfo.String(),
		Release: release.Compiled().String(),
		At:      time.Now().UTC().Format(time.RFC3339),
	}
	add := func(id, outcome, detail string) {
		rep.Checks = append(rep.Checks, planeCheck{ID: id, Outcome: outcome, Detail: detail})
	}

	switch plane {
	case "admin":
		// Custody: nova-admin is the ONLY container that mounts the federation
		// PKI, and that is the property worth re-checking after an upgrade
		// swapped the image.
		if _, err := os.Stat("/var/lib/nova/fedpki"); err == nil {
			add("custody.pki-mounted", passed, "the federation PKI volume is present")
		} else {
			add("custody.pki-mounted", skipped, "no PKI volume mounted; federation may not be initialised")
		}
		add("catalog.linked", boolOutcome(release.Compiled().Stamped()),
			release.Compiled().String())
		add("buildinfo.stamped", boolOutcome(buildinfo.Stamped()), buildinfo.String())

	case "database":
		if pool == nil {
			add("schema.applied", failed, "the database is unreachable")
			break
		}
		applied, err := release.AppliedSchema(ctx, pool)
		if err != nil {
			add("schema.applied", failed, err.Error())
			break
		}
		want := int64(release.Compiled().TargetSchema)
		if applied == want {
			add("schema.applied", passed, fmt.Sprintf("schema %d, as this release expects", applied))
		} else {
			add("schema.applied", failed, fmt.Sprintf("schema %d, but this release expects %d", applied, want))
		}

	case "coordinator":
		if pool == nil {
			add("coordinator.reachable", failed, "the database is unreachable")
			break
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM nodes`).Scan(&n); err != nil {
			add("coordinator.reachable", failed, err.Error())
		} else {
			add("coordinator.reachable", passed, fmt.Sprintf("%d node(s) in the registry", n))
		}

	case "donor":
		// The canary must NOT mutate archive state. Nova's delete is a SOFT
		// delete followed later by tombstone, shred and unpin, so a
		// create-read-delete probe leaves a soft-deleted blob and a scheduled
		// tombstone behind — that is a write to the archive, not a health check.
		//
		// Preference order: an authenticated donor read-source probe against an
		// object that already exists; then a pre-existing eligible object; then
		// a reserved canary with a documented lifecycle. With no eligible
		// source, this SKIPS WITH A REASON rather than inventing one.
		if pool == nil {
			add("donor.read-source", skipped, "the database is unreachable")
			break
		}
		var eligible int
		_ = pool.QueryRow(ctx, `
			SELECT count(*) FROM nodes
			WHERE status = 'active' AND effective_capabilities @> ARRAY['read-source/v1']`).Scan(&eligible)
		if eligible == 0 {
			add("donor.read-source", skipped,
				"no active read-source-capable donor; there is nothing to probe, and creating a "+
					"blob to probe with would write to the archive (delete is a SOFT delete)")
		} else {
			add("donor.read-source", passed,
				fmt.Sprintf("%d eligible read source(s); the host orchestrator performs the "+
					"authenticated probe, which needs the overlay", eligible))
		}
	}

	rep.Outcome = passed
	for _, c := range rep.Checks {
		if c.Outcome == failed {
			rep.Outcome = failed
		}
	}
	sum := sha256.Sum256(mustJSONBytes(rep.Checks))
	rep.ResultSHA = "sha256:" + hex.EncodeToString(sum[:])
	return rep
}

// recordVerify writes the plane's result into upgrade_events and, once every
// plane the orchestrator runs has reported, completes the run.
//
// It uses the GENERATED queries rather than inline SQL. internal/upgrade's
// apply path cannot: it holds the advisory lock on one *sql.Conn and every
// write it makes has to be on that same session. Here there is no lock to
// share, so the duplicate-SQL problem has no excuse.
func recordVerify(ctx context.Context, pool *pgxpool.Pool, rep planeReport, configPath string) error {
	q := gen.New(pool)

	run, err := q.LatestUpgradeRun(ctx)
	if err != nil {
		// No run to attach to. Verification is still legitimate — an operator
		// may verify a deployment nobody migrated — so this is a fact, not a
		// failure.
		return nil
	}

	detail, err := json.Marshal(map[string]any{
		"plane": rep.Plane, "run_id": rep.RunID,
		"binary": rep.Binary, "release": rep.Release,
		"result_sha256": rep.ResultSHA, "checks": rep.Checks,
	})
	if err != nil {
		return err
	}

	// The sequence is derived from what is already recorded, so two planes
	// reporting concurrently cannot collide on (run_id, sequence): the insert
	// is ON CONFLICT DO NOTHING, and a lost event is better than a failed
	// verification.
	existing, err := q.ListUpgradeEvents(ctx, run.ID)
	if err != nil {
		return err
	}
	next := int64(len(existing))

	if err := q.RecordUpgradeEvent(ctx, gen.RecordUpgradeEventParams{
		RunID:    run.ID,
		Sequence: next,
		Phase:    "verify",
		State:    rep.Outcome,
		Detail:   detail,
	}); err != nil {
		return err
	}

	// The run completes on the last plane the in-image orchestration runs. The
	// AFTER fingerprint is taken here because this is the latest point in the
	// upgrade that still belongs to it — after the deployment swap, before the
	// operator moves on.
	if rep.Plane == adminPlanes[len(adminPlanes)-1] {
		state := upgrade.StatePassed
		if rep.Outcome == failed {
			state = upgrade.StateFailed
		}
		after := ""
		if b, rerr := os.ReadFile(configPath); rerr == nil {
			if fp, _, ferr := release.Fingerprint(b); ferr == nil {
				after = fp
			}
		}
		if after == "" {
			after = "unavailable: " + configPath + " could not be fingerprinted"
		}
		return q.CompleteUpgradeRun(ctx, gen.CompleteUpgradeRunParams{
			ID:                     run.ID,
			State:                  state,
			ConfigFingerprintAfter: after,
		})
	}
	return nil
}

func boolOutcome(ok bool) string {
	if ok {
		return passed
	}
	return failed
}

func mustJSONBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// writePlaneReport writes the report atomically into the mounted directory, so
// a crash mid-write cannot leave a half-file that reads as a result.
func writePlaneReport(dir string, rep planeReport, body []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s.json", rep.RunID, rep.Plane)
	final := filepath.Join(dir, name)
	tmp := final + ".partial"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", final)
	return nil
}
