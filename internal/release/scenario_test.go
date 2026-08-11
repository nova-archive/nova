package release

import (
	"slices"
	"strings"
	"testing"
)

// TestScenarioPairingAcceptsCommitIdentity. The baseline has no product
// version — the remote tag namespace holds only two lightweight pre-contract
// refs — and the transition FROM it is the one that has to work. A scenario
// model that can only name versions cannot describe it.
func TestScenarioPairingAcceptsCommitIdentity(t *testing.T) {
	in := validIntent()
	table := []GateCoverage{{Gate: "g", Proves: []string{"c1"}}}
	s := []Scenario{{
		ID: "baseline-to-v0.3.0", Coordinator: "v0.3.0", Donor: "commit:143c459",
		Gate: "g", Claims: []string{"c1"},
	}}
	if err := ValidateScenarios(in, s, table); err != nil {
		t.Fatalf("a commit-identified donor must be a legal pairing side: %v", err)
	}

	bad := []Scenario{{ID: "x", Coordinator: "v0.3.0", Donor: "commit:zzz", Gate: "g"}}
	if err := ValidateScenarios(in, bad, table); err == nil {
		t.Error("a malformed commit identity must be rejected")
	}
}

// TestScenarioPairingRepresentsNMinus2: "previous donor" cannot express this,
// which is why pairings are exact.
func TestScenarioPairingRepresentsNMinus2(t *testing.T) {
	in := validIntent()
	table := []GateCoverage{{Gate: "g", Proves: []string{"c1"}}}
	s := []Scenario{
		{ID: "n-1", Coordinator: "v0.6.0", Donor: "v0.5.0", Gate: "g", Claims: []string{"c1"}},
		{ID: "n-2", Coordinator: "v0.6.0", Donor: "v0.4.0", Gate: "g", Claims: []string{"c1"}},
	}
	if err := ValidateScenarios(in, s, table); err != nil {
		t.Fatalf("N-1 and N-2 must be separately expressible: %v", err)
	}
}

// TestDeclaredClaimCarriesNoEvidence / TestProvenClaimRequiresSignedEvidence
// are the type-level halves of the same rule; see intent_test and lock_test.
// This one checks the coverage side: a claim beyond its gate's coverage is not
// a claim the gate can support, whatever the intent says.
func TestClaimBeyondGateCoverageIsRejected(t *testing.T) {
	in := validIntent() // one claim, c1, proven by upgrade-wire-e2e

	ok := []GateCoverage{{Gate: "upgrade-wire-e2e", Proves: []string{"c1"}}}
	if err := ValidateClaimCoverage(in, ok); err != nil {
		t.Fatalf("covered claim rejected: %v", err)
	}

	narrow := []GateCoverage{{Gate: "upgrade-wire-e2e", Proves: []string{"something-else"}}}
	err := ValidateClaimCoverage(in, narrow)
	if err == nil || !strings.Contains(err.Error(), "beyond") {
		t.Errorf("err = %v, want a coverage refusal", err)
	}

	if err := ValidateClaimCoverage(in, nil); err == nil {
		t.Error("a gate with no coverage entry proves nothing and must be refused")
	}
}

// TestPlaceholderCoverageBlocksReleaseCandidate. Placeholder coverage is the
// INTENDED shape, not an executed result; promoting on it would put a claim in
// a signed lock with nothing behind it.
func TestPlaceholderCoverageBlocksReleaseCandidate(t *testing.T) {
	err := ReleaseCandidateReady(Coverage())
	if err == nil {
		t.Fatal("the checked-in table still has placeholders (Tasks 22, 25, 26), so a release " +
			"candidate must be impossible right now")
	}
	for _, gate := range []string{"upgrade-wire-e2e", "upgrade-schema-e2e", "crossversion-e2e"} {
		if !strings.Contains(err.Error(), gate) {
			t.Errorf("the refusal does not name %s; an operator needs to know which", gate)
		}
	}

	settled := []GateCoverage{{Gate: "g", Proves: []string{"c1"}, Placeholder: false}}
	if err := ReleaseCandidateReady(settled); err != nil {
		t.Errorf("a fully-derived table must permit a candidate: %v", err)
	}
}

// TestEveryDeclaredClaimHasACoverageEntry ties the shipped intent to the
// shipped table, so adding a claim without describing its gate fails here
// rather than at release time.
func TestEveryDeclaredClaimHasACoverageEntry(t *testing.T) {
	b, in := readCommittedIntent(t)
	_ = b
	if err := ValidateClaimCoverage(in, Coverage()); err != nil {
		t.Fatal(err)
	}
}

// TestCoverageRecordsItsLimitsHonestly. An entry that lists only what it proves
// invites the reader to assume everything else. The cross-version gate is the
// case in point: its caveats belong to ONE binary, and recording them as
// universal would be the wrong lesson.
func TestCoverageRecordsItsLimitsHonestly(t *testing.T) {
	for _, c := range Coverage() {
		if len(c.DoesNotProve) == 0 {
			t.Errorf("gate %s records no limits; absence of an entry is not a statement", c.Gate)
		}
		if c.Placeholder && c.Note == "" {
			t.Errorf("gate %s is a placeholder with no explanation", c.Gate)
		}
		if c.Runner == "" {
			t.Errorf("gate %s declares no runner class; a static gate could then satisfy an "+
				"executed claim", c.Gate)
		}
	}

	cv, ok := CoverageFor("crossversion-e2e")
	if !ok {
		t.Fatal("the cross-version gate must be in the table; it exists and makes claims")
	}
	joined := strings.Join(cv.DoesNotProve, " ")
	for _, want := range []string{"FABRICATED", "provably broken", "stale"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the cross-version entry does not record %q; the gate's own header does", want)
		}
	}
	if slices.Contains(cv.Proves, "baseline-donor-interop") {
		t.Error("the cross-version gate is pinned three milestones behind the baseline and " +
			"cannot support a baseline claim until Task 22 repins and reruns it")
	}
}

func TestScenarioRejectsUndeclaredClaimAndUnknownGate(t *testing.T) {
	in := validIntent()
	table := []GateCoverage{{Gate: "g", Proves: []string{"c1"}}}

	undeclared := []Scenario{{ID: "s", Coordinator: "v0.3.0", Donor: "v0.2.0", Gate: "g", Claims: []string{"nope"}}}
	if err := ValidateScenarios(in, undeclared, table); err == nil {
		t.Error("a scenario cannot claim something the intent never declared")
	}

	unknownGate := []Scenario{{ID: "s", Coordinator: "v0.3.0", Donor: "v0.2.0", Gate: "ghost"}}
	if err := ValidateScenarios(in, unknownGate, table); err == nil {
		t.Error("a scenario cannot name a gate with no coverage entry")
	}

	dup := []Scenario{
		{ID: "s", Coordinator: "v0.3.0", Donor: "v0.2.0", Gate: "g"},
		{ID: "s", Coordinator: "v0.3.0", Donor: "v0.1.0", Gate: "g"},
	}
	if err := ValidateScenarios(in, dup, table); err == nil {
		t.Error("duplicate scenario ids make evidence references ambiguous")
	}
}
