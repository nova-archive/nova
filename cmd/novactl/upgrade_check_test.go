package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Task 20 — `upgrade check`
// ---------------------------------------------------------------------------

// TestCheckRequiresAVerifiedTargetLock. Preflight evaluates a target the
// operator has ALREADY verified out of band (Task 17). A path is not an
// identity, so all three inputs are mandatory and the digest is checked.
func TestCheckRequiresAVerifiedTargetLock(t *testing.T) {
	intentPath, lockPath, dgst := rolloutFixture(t)

	for _, args := range [][]string{
		{"check", "--intent", intentPath, "--expect-lock-digest", dgst}, // no lock
		{"check", "--lock", lockPath, "--expect-lock-digest", dgst},     // no intent
		{"check", "--lock", lockPath, "--intent", intentPath},           // no digest
	} {
		if err := cmdUpgrade(args); err == nil {
			t.Errorf("%v was accepted with an input missing", args)
		}
	}

	wrong := "sha256:" + strings.Repeat("f", 64)
	if _, err := loadVerifiedLock(lockPath, intentPath, wrong); err == nil {
		t.Fatal("a lock whose bytes do not hash to the verified digest must be refused")
	}
}

// TestCheckNeverFetchesMetadata (T1.22). The proof is structural: the only
// inputs are a compiled-in catalog, files on disk and the local database. Any
// HTTP client in the preflight path would be a phone-home, so the source is
// checked for one.
func TestCheckNeverFetchesMetadata(t *testing.T) {
	for _, f := range []string{"upgrade.go", "upgrade_check.go", "upgrade_status.go", "upgrade_verify.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{`"net/http"`, `"net/url"`, "http.Get", "http.Client"} {
			if bytes.Contains(b, []byte(forbidden)) {
				t.Errorf("%s references %s; T1.22 forbids a binary reaching out for release metadata",
					f, forbidden)
			}
		}
	}
}

// TestCheckPrintsRollbackBoundaryFirst. It is the one answer an operator needs
// BEFORE starting rather than after failing, and a list of passes above it is
// how it gets read afterwards instead.
func TestCheckPrintsRollbackBoundaryFirst(t *testing.T) {
	rep := checkReport{}
	rep.Target.Version = "v0.3.0"
	rep.RollbackBoundary = "redeploy the previous binary"
	rep.Checks = []checkResult{{ID: "required-secrets", Requirement: required, Outcome: passed}}

	out, _ := captureStdout(t, func() error { printCheck(rep); return nil })
	first, _, _ := strings.Cut(out, "\n")
	if !strings.Contains(first, rep.RollbackBoundary) {
		t.Errorf("first line = %q, want the rollback boundary", first)
	}
}

// TestCheckMutatesNothing. An operator will run preflight twice; preflight that
// changes state cannot be run twice. runPreflight takes a pool and must issue
// only reads, so the source is checked for write verbs.
func TestCheckMutatesNothing(t *testing.T) {
	b, err := os.ReadFile("upgrade_check.go")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ToUpper(string(b))
	for _, verb := range []string{"INSERT INTO", "UPDATE ", "DELETE FROM", "ALTER TABLE", "POOL.EXEC"} {
		if strings.Contains(src, verb) {
			t.Errorf("upgrade_check.go contains %q; preflight must be re-runnable, which means "+
				"read-only", verb)
		}
	}
}

// TestCheckSkipsWithReasonWhenDatabaseUnreachable. A skip carries the reason,
// because "could not check" without a reason is indistinguishable from "fine".
func TestCheckSkipsWithReasonWhenDatabaseUnreachable(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	intentPath, lockPath, dgst := rolloutFixture(t)
	lock, err := loadVerifiedLock(lockPath, intentPath, dgst)
	if err != nil {
		t.Fatal(err)
	}

	rep := runPreflight(t.Context(), nil, lock, dgst, filepath.Join(t.TempDir(), "absent.yaml"), nil)

	byID := map[string]checkResult{}
	for _, c := range rep.Checks {
		byID[c.ID] = c
		if c.Outcome == skipped && strings.TrimSpace(c.Detail) == "" {
			t.Errorf("check %q skipped with no reason", c.ID)
		}
	}
	for _, id := range []string{"rollback-boundary", "fleet-compatibility"} {
		if byID[id].Outcome != skipped {
			t.Errorf("%s = %q with no database, want %q", id, byID[id].Outcome, skipped)
		}
	}
	if !rep.Blocked {
		t.Error("required checks were skipped and not acknowledged; that must block")
	}
}

// TestRequiredSkippedBlocksUnlessAcknowledged. Requirement and outcome are
// separate axes: a single enum in which "could not be checked" reads the same
// as "is fine" silently permits a dangerous migration.
func TestRequiredSkippedBlocksUnlessAcknowledged(t *testing.T) {
	c := checkResult{ID: "disk-headroom", Requirement: required, Outcome: skipped}
	if !c.blocks(nil) {
		t.Error("a required check that could not run must block")
	}
	if c.blocks([]string{"disk-headroom"}) {
		t.Error("acknowledging it by id is the operator saying they checked; that must unblock")
	}
	if !c.blocks([]string{"fleet-compatibility"}) {
		t.Error("acknowledging a DIFFERENT id must not unblock this one")
	}

	opt := checkResult{ID: "config-keys", Requirement: optional, Outcome: skipped}
	if opt.blocks(nil) {
		t.Error("an optional check that could not run is not a blocker")
	}

	// A FAILURE is not waivable. Acknowledgement means "I checked this myself",
	// which an operator can say about disk headroom and cannot say about a
	// measured incompatibility.
	f := checkResult{ID: "fleet-compatibility", Requirement: required, Outcome: failed}
	if !f.blocks(nil) {
		t.Error("a failed required check must block")
	}
	if !f.blocks([]string{"fleet-compatibility"}) {
		t.Error("acknowledgement waives a SKIP, never a measured failure")
	}
}

// TestCheckDoesNotBlockOnReportedVersionAlone. The interop contract is
// negotiated protocol plus capabilities. A version STRING is an untrusted
// self-report, and blocking on it would make the support window a weapon rather
// than a statement.
func TestCheckDoesNotBlockOnReportedVersionAlone(t *testing.T) {
	b, err := os.ReadFile("upgrade_check.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	// fleetOutcome counts unsupported donors so it can SAY so, and must not use
	// that count in either failing branch.
	body := src[strings.Index(src, "func fleetOutcome"):]
	body = body[:strings.Index(body, "\nfunc ")]
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "return failed") && strings.Contains(line, "unsupported") {
			t.Errorf("fleetOutcome fails on an unsupported version: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(body, "if incompatible > 0") {
		t.Error("fleetOutcome must block on protocol incompatibility, which is the real contract")
	}
	if !strings.Contains(body, "if debt > 0") {
		t.Error("fleetOutcome must block on unresolved drain debt")
	}
}
