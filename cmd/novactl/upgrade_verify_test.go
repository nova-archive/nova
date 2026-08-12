package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Task 21 — `upgrade verify`
// ---------------------------------------------------------------------------

// TestVerifyRunsPerPlaneUnderOneRunID. Every plane's evidence has to join to one
// run, or the aggregate is a pile of unrelated results.
func TestVerifyRunsPerPlaneUnderOneRunID(t *testing.T) {
	const runID = "2026-08-11T00-00-00Z-abcdef"
	for _, plane := range adminPlanes {
		rep := verifyPlane(t.Context(), nil, plane, runID)
		if rep.RunID != runID {
			t.Errorf("plane %s carried run id %q, want %q", plane, rep.RunID, runID)
		}
		if rep.Plane != plane {
			t.Errorf("plane %s reported itself as %q", plane, rep.Plane)
		}
		if rep.ResultSHA == "" {
			t.Errorf("plane %s produced no hashed result for the orchestrator to aggregate", plane)
		}
	}

	if err := cmdUpgradeVerify([]string{"--plane", "admin"}); err == nil {
		t.Error("a missing --run-id must be an error, not an orphan report")
	}
	for _, plane := range hostPlanes {
		err := cmdUpgradeVerify([]string{"--plane", plane, "--run-id", runID})
		if err == nil || !strings.Contains(err.Error(), "runs on the host") {
			t.Errorf("plane %q: err = %v, want a refusal naming the host", plane, err)
		}
	}
	if err := cmdUpgradeVerify([]string{"--plane", "nonsense", "--run-id", runID}); err == nil {
		t.Error("an unknown plane must be an error rather than a silent empty pass")
	}
}

// TestVerifyWritesLocalReportWhenDatabaseUnreachable. nova-admin runs `--rm`, so
// a report on its root filesystem evaporates exactly when it is most needed.
func TestVerifyWritesLocalReportWhenDatabaseUnreachable(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	dir := t.TempDir()

	err := cmdUpgradeVerify([]string{"--plane", "database", "--run-id", "r7", "--report-dir", dir})
	if err == nil {
		t.Fatal("the database plane cannot pass with no database")
	}

	body, readErr := os.ReadFile(filepath.Join(dir, "r7-database.json"))
	if readErr != nil {
		t.Fatalf("the failing report was not persisted: %v", readErr)
	}
	var rep planeReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != failed {
		t.Errorf("persisted outcome = %q, want %q", rep.Outcome, failed)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "*.partial")); len(entries) > 0 {
		t.Errorf("a partial file survived: %v — the write must be atomic", entries)
	}
}

// TestCanaryDoesNotMutateArchiveState. Nova's delete is a SOFT delete followed
// later by tombstone, shred and unpin, so create→read→delete leaves a
// soft-deleted blob and a scheduled tombstone behind. That is a write to the
// archive, not a health check.
func TestCanaryDoesNotMutateArchiveState(t *testing.T) {
	b, err := os.ReadFile("upgrade_verify.go")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ToUpper(string(b))
	for _, verb := range []string{"INSERT INTO", "UPDATE ", "DELETE FROM"} {
		if strings.Contains(src, verb) {
			t.Errorf("upgrade_verify.go contains %q; the canary must not write to the archive", verb)
		}
	}
}

// TestVerifySkipsDonorBackedReadWithNoEligibleSource. With nothing to probe it
// SKIPS WITH A REASON rather than inventing a source to probe with.
func TestVerifySkipsDonorBackedReadWithNoEligibleSource(t *testing.T) {
	rep := verifyPlane(t.Context(), nil, "donor", "r1")
	if len(rep.Checks) != 1 {
		t.Fatalf("checks = %+v, want exactly the read-source probe", rep.Checks)
	}
	c := rep.Checks[0]
	if c.Outcome != skipped {
		t.Errorf("outcome = %q, want %q", c.Outcome, skipped)
	}
	if strings.TrimSpace(c.Detail) == "" {
		t.Error("a skip with no reason is indistinguishable from a pass")
	}
	if rep.Outcome != passed {
		t.Errorf("a skipped probe must not fail the plane; outcome = %q", rep.Outcome)
	}
}

// TestPlaneReportIsSelfDescribing. The orchestrator aggregates these across
// planes and releases, so each has to say which binary and release produced it.
func TestPlaneReportIsSelfDescribing(t *testing.T) {
	rep := verifyPlane(t.Context(), nil, "admin", "r1")
	if rep.Binary == "" || rep.Release == "" || rep.At == "" {
		t.Errorf("report is not self-describing: %+v", rep)
	}
	if !strings.HasPrefix(rep.ResultSHA, "sha256:") {
		t.Errorf("result hash = %q, want a sha256: prefix", rep.ResultSHA)
	}
}
