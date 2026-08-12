package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/db/migrations"
)

// Tests for the `novactl upgrade` surface (P2-M7.3, Tasks 19–21).
//
// Every test here runs WITHOUT a database on purpose. That is not a
// convenience: "the database is unreachable" is a first-class state these
// commands are required to handle, because it is one of the moments an operator
// most wants to ask what is running.

// ---------------------------------------------------------------------------
// Task 19 — `upgrade status`
// ---------------------------------------------------------------------------

// TestUpgradeSubcommandsParse pins the flag surface. --check-flags stops each
// subcommand the instant its flags parse, so this reaches all three without a
// database, a lock file or a run id.
func TestUpgradeSubcommandsParse(t *testing.T) {
	checkFlagsOnly = true
	t.Cleanup(func() { checkFlagsOnly = false })

	for _, sub := range [][]string{
		{"status"},
		{"status", "--json"},
		{"check", "--lock", "/tmp/l.json", "--intent", "/tmp/i.json",
			"--expect-lock-digest", "sha256:x", "--acknowledge", "disk-headroom"},
		{"verify", "--plane", "admin", "--run-id", "r1", "--report-dir", "/tmp/r"},
	} {
		if err := cmdUpgrade(sub); !errors.Is(err, errCheckFlagsOK) {
			t.Errorf("cmdUpgrade(%v) = %v, want the flags to parse and stop short of any work",
				sub, err)
		}
	}
}

func TestUpgradeRejectsAnUnknownSubcommand(t *testing.T) {
	if err := cmdUpgrade([]string{"rollback"}); err == nil {
		t.Fatal("an unknown subcommand must be an error, not a silent no-op")
	}
	if err := cmdUpgrade(nil); err == nil {
		t.Fatal("bare `novactl upgrade` must print usage as an error")
	}
}

// TestStatusDegradesGracefullyWithoutDatabase. Binary identity and the embedded
// catalog are compiled in and always answerable; everything that needs the
// database reports `unavailable`, which is a different statement from "none".
func TestStatusDegradesGracefullyWithoutDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	rep := buildStatus(t.Context(), optionalPool(t.Context()))

	if rep.Binary.Version == "" {
		t.Error("binary version is compiled in and must always be reported")
	}
	if rep.Schema.Available || rep.Fleet.Available || rep.LastRun.Available {
		t.Error("no database was reachable, so no database-derived section may claim availability")
	}
	for name, got := range map[string]string{
		"applied": rep.Schema.Applied, "pending": rep.Schema.Pending,
		"boundary": rep.Schema.Boundary,
	} {
		if got != unavailable {
			t.Errorf("schema.%s = %q, want %q — \"could not find out\" is not \"nothing\"",
				name, got, unavailable)
		}
	}

	// And it must render, since printing is the whole command.
	out, _ := captureStdout(t, func() error { printStatus(rep); return nil })
	if !strings.Contains(out, unavailable) {
		t.Errorf("printed status never says %q:\n%s", unavailable, out)
	}
}

// TestStatusJSONRoundTrips guards the machine-readable contract other tooling
// consumes.
func TestStatusJSONRoundTrips(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	rep := buildStatus(t.Context(), nil)

	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back statusReport
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Binary.Version != rep.Binary.Version {
		t.Errorf("round trip lost the binary version: %q != %q", back.Binary.Version, rep.Binary.Version)
	}
}

// TestRollbackBoundaryNamesItsPredecessor. "redeploy the previous binary" and
// "restore from backup" are two very different days, and the sentence has to
// say which one AND name the binary it means — "the previous binary" is not an
// instruction anybody can follow at 3am.
func TestRollbackBoundaryNamesItsPredecessor(t *testing.T) {
	pred := []string{"commit:143c459"}

	for _, tc := range []struct {
		name string
		b    migrations.Boundary
		want string
	}{
		{"a destructive migration needs the backup",
			migrations.Boundary{RestoreToRevert: true, OldBinaryCompatible: true, RelativeTo: pred},
			"RESTORE FROM BACKUP"},
		{"an incompatible range needs the backup even when nothing was destroyed",
			migrations.Boundary{RestoreToRevert: false, OldBinaryCompatible: false, RelativeTo: pred},
			"RESTORE FROM BACKUP"},
		{"a compatible, non-destructive range is a redeploy",
			migrations.Boundary{RestoreToRevert: false, OldBinaryCompatible: true, RelativeTo: pred},
			"redeploy"},
	} {
		got := rollbackBoundary(tc.b)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q does not say %q", tc.name, got, tc.want)
		}
		if !strings.Contains(got, pred[0]) {
			t.Errorf("%s: %q does not name the predecessor it is relative to", tc.name, got)
		}
	}
}
