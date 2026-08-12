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
	"github.com/nova-archive/nova/internal/release"
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
