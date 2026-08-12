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

// TestReleaseCandidateIsReadyAndCompletionIsNot.
//
// These are DIFFERENT questions, and conflating them is what made a first
// release impossible. A lock can be cut today: every gate a claim depends on
// has derived coverage. The milestone is NOT done, because the transition to a
// published release has not been demonstrated — and it cannot be until one is
// published.
//
// Requiring the second at lock time would have required a release to prove
// something about itself before it existed.
func TestReleaseCandidateIsReadyAndCompletionIsNot(t *testing.T) {
	if err := ReleaseCandidateReady(Coverage()); err != nil {
		t.Fatalf("a release candidate is blocked: %v", err)
	}

	err := CompletionReady(Coverage())
	if err == nil {
		t.Fatal("completion state 4 reports reached, but upgrade-release-e2e has not run. " +
			"If it genuinely has, retire its placeholder and mark the ROADMAP row complete")
	}
	if !strings.Contains(err.Error(), "upgrade-release-e2e") {
		t.Errorf("the refusal does not name the outstanding gate: %v", err)
	}

	// A placeholder on a PRE-publication gate must still block a candidate.
	// This is the half that has to keep working.
	blocked := Coverage()
	for i := range blocked {
		if !blocked[i].PostPublication {
			blocked[i].Placeholder = true
			break
		}
	}
	if err := ReleaseCandidateReady(blocked); err == nil {
		t.Error("a pre-publication placeholder no longer blocks a release candidate")
	}

	settled := []GateCoverage{{Gate: "g", Proves: []string{"c1"}}}
	if err := ReleaseCandidateReady(settled); err != nil {
		t.Errorf("a fully-derived table must permit a candidate: %v", err)
	}
	if err := CompletionReady(settled); err != nil {
		t.Errorf("a table with no post-publication gate is complete: %v", err)
	}
}

// TestNoClaimIsBoundToAPostPublicationGate. The lock is signed before such a
// gate can run, so a claim bound to one could never be proven — which is
// exactly the bootstrap that made a first release impossible.
func TestNoClaimIsBoundToAPostPublicationGate(t *testing.T) {
	_, in := readCommittedIntent(t)
	if err := ValidateClaimCoverage(in, Coverage()); err != nil {
		t.Fatal(err)
	}

	post := ""
	for _, c := range Coverage() {
		if c.PostPublication {
			post = c.Gate
			break
		}
	}
	if post == "" {
		t.Skip("no post-publication gate in the table")
	}
	bad := in
	bad.Claims = append([]DeclaredClaim(nil), in.Claims...)
	bad.Claims[0].ProvenByGate = post
	if err := ValidateClaimCoverage(bad, Coverage()); err == nil {
		t.Fatal("an intent binding a claim to a post-publication gate was accepted")
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
	if cv.Placeholder {
		t.Error("the cross-version entry is derived from the 2026-08-11 run against 143c459")
	}

	joined := strings.Join(cv.DoesNotProve, " ") + " " + cv.Note
	// The old caveats described the P2-M6 binary this gate used to be pinned to.
	// Carrying them forward would attribute one binary's defects to every
	// predecessor, which is the wrong lesson and the reason this test exists.
	for _, gone := range []string{"three milestones", "provably broken"} {
		if strings.Contains(strings.ToLower(joined), strings.ToLower(gone)) &&
			!strings.Contains(joined, "described the P2-M6 binary") &&
			!strings.Contains(joined, "not known broken") {
			t.Errorf("the entry still asserts %q about the current predecessor", gone)
		}
	}
	// It must still be explicit about the two things it genuinely cannot say.
	for _, want := range []string{"FRESH database", "built from source"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the entry does not record the %q limit", want)
		}
	}
	if slices.Contains(cv.Proves, "baseline-donor-interop") {
		t.Error("baseline-donor-interop is assigned to upgrade-wire-e2e. This gate overlaps it " +
			"heavily, and re-pointing a reviewed claim at whichever gate happens to cover it " +
			"is how coverage stops meaning anything")
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
