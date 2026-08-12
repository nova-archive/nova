package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/buildinfo"
	"github.com/nova-archive/nova/internal/db/migrations"
	"github.com/nova-archive/nova/internal/release"
)

// `novactl upgrade status` answers "what is this deployment, right now" with
// no network and, if need be, no database.

type statusReport struct {
	Binary struct {
		Version   string `json:"version"`
		Revision  string `json:"revision"`
		BuildDate string `json:"build_date"`
		Stamped   bool   `json:"stamped"`
	} `json:"binary"`
	Release struct {
		Version      string `json:"version"`
		IntentDigest string `json:"intent_digest"`
		TargetSchema int    `json:"target_schema"`
		Generated    bool   `json:"generated"`
	} `json:"release"`
	Schema struct {
		Applied   string `json:"applied"`
		Pending   string `json:"pending"`
		Boundary  string `json:"rollback_boundary"`
		Available bool   `json:"available"`
	} `json:"schema"`
	Fleet struct {
		Nodes         int    `json:"nodes"`
		OldestVersion string `json:"oldest_version,omitempty"`
		Available     bool   `json:"available"`
	} `json:"fleet"`
	LastRun struct {
		Release   string `json:"release,omitempty"`
		State     string `json:"state,omitempty"`
		StartedAt string `json:"started_at,omitempty"`
		Available bool   `json:"available"`
	} `json:"last_upgrade_run"`
}

func cmdUpgradeStatus(args []string) error {
	fs := flag.NewFlagSet("upgrade status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	ctx := context.Background()
	rep := buildStatus(ctx, optionalPool(ctx))

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printStatus(rep)
	return nil
}

func buildStatus(ctx context.Context, pool *pgxpool.Pool) statusReport {
	var rep statusReport

	// Always available: this is compiled in, which is the whole point.
	rep.Binary.Version = buildinfo.Version()
	rep.Binary.Revision = buildinfo.Revision()
	rep.Binary.BuildDate = buildinfo.BuildDate()
	rep.Binary.Stamped = buildinfo.Stamped()

	cat := release.Compiled()
	rep.Release.Version = cat.Version
	rep.Release.IntentDigest = cat.IntentDigest
	rep.Release.TargetSchema = cat.TargetSchema
	rep.Release.Generated = cat.Stamped()

	rep.Schema.Applied = unavailable
	rep.Schema.Pending = unavailable
	rep.Schema.Boundary = unavailable
	if pool == nil {
		return rep
	}
	defer pool.Close()

	applied, err := release.AppliedSchema(ctx, pool)
	if err == nil {
		rep.Schema.Available = true
		rep.Schema.Applied = fmt.Sprintf("%d", applied)

		b, berr := migrations.Between(applied, int64(cat.TargetSchema))
		switch {
		case berr != nil:
			rep.Schema.Pending = berr.Error()
		case applied == int64(cat.TargetSchema):
			rep.Schema.Pending = "none"
			rep.Schema.Boundary = "n/a — nothing to apply"
		default:
			rep.Schema.Pending = fmt.Sprintf("%d migration(s), (%d, %d]",
				int(b.To-b.From), b.From, b.To)
			rep.Schema.Boundary = rollbackBoundary(b)
		}
	}

	if rows, err := release.ReadCensus(ctx, pool); err == nil {
		rep.Fleet.Available = true
		rep.Fleet.Nodes = len(rows)
		axes := make([]release.Axes, 0, len(rows))
		for _, r := range rows {
			axes = append(axes, release.Classify(cat, r, time.Now(), time.Hour))
		}
		rep.Fleet.OldestVersion = release.OldestByVersion(axes)
	}

	if run, err := lastUpgradeRun(ctx, pool); err == nil {
		rep.LastRun = run
	}
	return rep
}

// rollbackBoundary renders the one sentence an operator most needs before
// starting: whether going back is a redeploy or a restore.
//
// Every branch NAMES the predecessor. "redeploy the previous binary" is not an
// instruction anyone can follow at 3am, and "restore from backup" without
// saying what you would be going back to leaves the operator guessing at the
// only decision that matters.
func rollbackBoundary(b migrations.Boundary) string {
	switch {
	case b.RestoreToRevert:
		return fmt.Sprintf("RESTORE FROM BACKUP — a migration in this range destroys information "+
			"going forward, so %v cannot be reached by redeploying it", b.RelativeTo)
	case !b.OldBinaryCompatible:
		return fmt.Sprintf("RESTORE FROM BACKUP — this range is not compatible with %v",
			b.RelativeTo)
	default:
		return fmt.Sprintf("redeploy %v — the schema stays forward and that binary runs against it",
			b.RelativeTo)
	}
}

func lastUpgradeRun(ctx context.Context, pool *pgxpool.Pool) (r struct {
	Release   string `json:"release,omitempty"`
	State     string `json:"state,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	Available bool   `json:"available"`
}, err error) {
	var rel, state string
	var started time.Time
	err = pool.QueryRow(ctx,
		`SELECT to_release, state, started_at FROM upgrade_runs ORDER BY started_at DESC LIMIT 1`).
		Scan(&rel, &state, &started)
	if err != nil {
		// No table (pre-0019) or no rows: nothing recorded, which is a fact
		// rather than a failure.
		return r, err
	}
	r.Available = true
	r.Release, r.State = rel, state
	r.StartedAt = started.Format(time.RFC3339)
	return r, nil
}

func printStatus(rep statusReport) {
	fmt.Println("binary")
	fmt.Printf("  version:        %s\n", rep.Binary.Version)
	fmt.Printf("  revision:       %s\n", rep.Binary.Revision)
	fmt.Printf("  built:          %s\n", rep.Binary.BuildDate)
	if !rep.Binary.Stamped {
		fmt.Println("  note:           unstamped build (go run / bare go build)")
	}

	fmt.Println("release")
	if rep.Release.Generated {
		fmt.Printf("  version:        %s\n", rep.Release.Version)
		fmt.Printf("  intent digest:  %s\n", rep.Release.IntentDigest)
		fmt.Printf("  target schema:  %d\n", rep.Release.TargetSchema)
	} else {
		fmt.Println("  catalog:        not generated (run `make catalog`)")
	}

	fmt.Println("schema")
	fmt.Printf("  applied:        %s\n", rep.Schema.Applied)
	fmt.Printf("  pending:        %s\n", rep.Schema.Pending)
	fmt.Printf("  going back:     %s\n", rep.Schema.Boundary)

	fmt.Println("fleet")
	if rep.Fleet.Available {
		fmt.Printf("  donors:         %d\n", rep.Fleet.Nodes)
		if rep.Fleet.OldestVersion != "" {
			fmt.Printf("  oldest version: %s\n", rep.Fleet.OldestVersion)
		}
	} else {
		fmt.Printf("  donors:         %s\n", unavailable)
	}

	fmt.Println("last upgrade run")
	if rep.LastRun.Available {
		fmt.Printf("  %s → %s (%s)\n", rep.LastRun.StartedAt, rep.LastRun.Release, rep.LastRun.State)
	} else {
		fmt.Printf("  %s\n", unavailable)
	}
}
