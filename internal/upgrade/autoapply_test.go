package upgrade

import (
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/db/migrations"
)

// TestAutoApplyRequiresEveryDimension. Safe unattended is a CONJUNCTION. A
// majority is not safety: one destructive migration in an otherwise additive
// range is still a destructive range, and the container has nobody watching.
func TestAutoApplyRequiresEveryDimension(t *testing.T) {
	safe := migrations.Boundary{
		From: 18, To: 19, OldBinaryCompatible: true, OnlineApplicable: true,
		RelativeTo: []string{"0018"},
	}
	if d := AutoApplicable(safe); !d.Apply {
		t.Fatalf("an old-binary-compatible, online, maintenance-free range must apply: %s", d.Reason)
	}

	for name, mutate := range map[string]func(*migrations.Boundary){
		"not old-binary compatible": func(b *migrations.Boundary) { b.OldBinaryCompatible = false },
		"not online applicable":     func(b *migrations.Boundary) { b.OnlineApplicable = false },
		"needs maintenance":         func(b *migrations.Boundary) { b.RequiresMaintenance = true },
		"restore to revert":         func(b *migrations.Boundary) { b.RestoreToRevert = true },
		"has a procedure": func(b *migrations.Boundary) {
			b.Procedures = []string{"Back up the database."}
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := safe
			mutate(&b)
			d := AutoApplicable(b)
			if d.Apply {
				t.Fatalf("a range that %s must not be applied unattended", name)
			}
			if d.Reason == "" {
				t.Error("the refusal has no reason, and the operator only sees container logs")
			}
			if d.Command == "" {
				t.Error("the refusal does not say what to run instead")
			}
			if !strings.Contains(d.Command, "--to 19") {
				t.Errorf("the command does not carry the target: %q", d.Command)
			}
		})
	}
}

// TestAutoApplyCommandCarriesEveryAcknowledgement. Printing "run migrate apply"
// without the acknowledgements means the operator's first attempt fails and
// they have to read the error to assemble the real command.
func TestAutoApplyCommandCarriesEveryAcknowledgement(t *testing.T) {
	b := migrations.Boundary{
		From: 2, To: 3, OnlineApplicable: true, RestoreToRevert: true,
		Procedures: []string{"Back up the database.", "Stop the orchestrator."},
	}
	d := AutoApplicable(b)
	if d.Apply {
		t.Fatal("this range must not apply unattended")
	}
	for _, p := range b.Procedures {
		if !strings.Contains(d.Command, ObligationID(p)) {
			t.Errorf("the command omits %q:\n    %s", ObligationID(p), d.Command)
		}
	}
}

// TestFreshInstallIsAutoApplicable. 0001 declares RestoreToRevert and no
// compatible predecessor — both true, both vacuous on an empty database. If
// they blocked, no Nova deployment could ever start.
func TestFreshInstallIsAutoApplicable(t *testing.T) {
	b, err := migrations.Between(0, 19)
	if err != nil {
		t.Fatal(err)
	}
	if !b.BootstrapOnly {
		t.Fatal("a range from 0 must contain the bootstrap migration")
	}
	if d := AutoApplicable(b); !d.Apply {
		t.Fatalf("a fresh install must be auto-applicable: %s", d.Reason)
	}

	// The same obligations on an UPGRADE range are not vacuous, and must block.
	up, err := migrations.Between(2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if d := AutoApplicable(up); d.Apply {
		t.Fatal("(2, 3] drops two tables; it must never be applied unattended")
	}
}

// TestNothingToDoIsNotAnApply.
func TestNothingToDoIsNotAnApply(t *testing.T) {
	d := AutoApplicable(migrations.Boundary{From: 19, To: 19,
		OldBinaryCompatible: true, OnlineApplicable: true})
	if d.Apply {
		t.Error("an empty range is not something to apply")
	}
	if d.Command != "" {
		t.Errorf("there is nothing for the operator to run: %q", d.Command)
	}
}

// TestParseBoolRefusesATypo. `os.Getenv(x) == "true"` is what gets written
// instead, and under it NOVA_MIGRATE_ON_START=flase silently means false.
func TestParseBoolRefusesATypo(t *testing.T) {
	for _, raw := range []string{"flase", "ture", "yeah", "2", "disabled"} {
		if _, err := ParseBool("NOVA_MIGRATE_ON_START", raw, true); err == nil {
			t.Errorf("%q was accepted as a boolean", raw)
		}
	}
	for raw, want := range map[string]bool{
		"true": true, "TRUE": true, "yes": true, "on": true, "1": true, " t ": true,
		"false": false, "FALSE": false, "no": false, "off": false, "0": false,
	} {
		got, err := ParseBool("X", raw, true)
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %t, want %t", raw, got, want)
		}
	}
	if got, err := ParseBool("X", "", true); err != nil || !got {
		t.Errorf("an unset variable takes the default: %t %v", got, err)
	}
}

// TestObligationIDsAreUniqueAcrossEveryRealProcedure.
//
// Two of the shipped procedures both begin "Expect a backfill proportional to
// the blob count", so a truncated slug alone gives them the SAME handle and
// acknowledging one silently acknowledges the other. That is the bug this test
// exists to keep fixed.
func TestObligationIDsAreUniqueAcrossEveryRealProcedure(t *testing.T) {
	seen := map[string]string{}
	for _, o := range migrations.All() {
		for _, p := range o.Procedures {
			id := ObligationID(p)
			if prev, dup := seen[id]; dup && prev != p {
				t.Errorf("%s names two different procedures:\n    %s\n    %s", id, prev, p)
			}
			seen[id] = p
			if len(id) > 48 {
				t.Errorf("%s is too long to type", id)
			}
			if strings.ContainsAny(id, " \t'\"") {
				t.Errorf("%q needs shell quoting, which an operator will get wrong", id)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no procedures at all; this test is not checking anything")
	}
}

// TestObligationIDIsStableForTheSameSentence. An id that changes between the
// preflight that printed it and the apply that reads it is not a handle.
func TestObligationIDIsStableForTheSameSentence(t *testing.T) {
	const p = "Back up the database. This migration DROPS integrity_audits and audit_log."
	if ObligationID(p) != ObligationID(p) {
		t.Fatal("the id is not deterministic")
	}
	if ObligationID(p) == ObligationID(p+" ") {
		t.Error("a trailing space must produce a different id, or the digest is not covering " +
			"the whole sentence")
	}
}
