package release

import (
	"fmt"
	"slices"
	"strings"
)

// Compatibility scenarios and gate coverage (P2-M7.3, D-M7.3-12).
//
// # A scenario is an EXACT artifact pairing
//
// Not "previous donor". That phrasing cannot represent N-2, cannot represent a
// six-month-supported artifact that is no longer an immediate predecessor, and
// cannot represent the baseline at all — the baseline has no product version,
// only a commit. Since the baseline transition is the one that actually has to
// work, a model that cannot name it is not a model of Nova's releases.
//
// # Coverage is what a gate PROVES, and what it does not
//
// The cross-version gate's own header records that the pinned N-1 coordinator's
// audit passes are fabricated without reaching the donor, and its donor-backed
// reads are provably broken. Those are properties of THAT BINARY, not of
// previous coordinators generally — encoding them as a universal limitation
// would be the wrong lesson. So coverage is per-gate, checked in, and a claim
// beyond a gate's coverage is refused.
//
// Entries whose coverage has not yet been derived from an executed run are
// marked Placeholder. A release candidate is impossible while any remains.

// RunnerClass is the tier a gate runs in. A static gate must never satisfy a
// claim that needs an executed one.
type RunnerClass string

const (
	// RunnerStatic is the hermetic PR tier: no Docker, no TUN.
	RunnerStatic RunnerClass = "pr-static"
	// RunnerDocker is the RC tier: Docker available.
	RunnerDocker RunnerClass = "rc-docker"
	// RunnerTUN is the release tier: Docker plus /dev/net/tun.
	RunnerTUN RunnerClass = "release-tun"
)

// GateCoverage records what one gate demonstrates.
type GateCoverage struct {
	// Gate is the name a DeclaredClaim's ProvenByGate refers to.
	Gate string
	// Runner is the tier it runs in.
	Runner RunnerClass
	// Proves lists the claim IDs this gate can support.
	Proves []string
	// DoesNotProve records the honest limits, so a reader is not left to infer
	// them from the absence of an entry.
	DoesNotProve []string
	// Placeholder marks coverage that has NOT been derived from an executed
	// run. It blocks a release candidate; see ReleaseCandidateReady.
	Placeholder bool
	// Note explains a placeholder or a surprising limit.
	Note string
}

// Scenario is an exact coordinator/donor artifact pairing.
type Scenario struct {
	// ID is stable and human-readable.
	ID string
	// Coordinator and Donor are artifact identities: a product version, or
	// "commit:<sha>" for the precontract baseline.
	Coordinator string
	Donor       string
	// Claims are the claim IDs this pairing demonstrates.
	Claims []string
	// Gate is the gate that executes this pairing.
	Gate string
}

// coverage is the checked-in table.
//
// It lands with the CURRENT gates' coverage. Task 22 repins the cross-version
// gate to the baseline commit and reruns it, and replaces the placeholders with
// values derived from that run — which is what makes the coverage honest rather
// than aspirational.
var coverage = []GateCoverage{
	{
		Gate: "upgrade-wire-e2e", Runner: RunnerDocker,
		Proves: []string{"baseline-donor-interop"},
		DoesNotProve: []string{
			"anything about the schema: this gate runs against a FRESH database, so it " +
				"cannot say whether an old binary survives a forward schema",
			"anything about released artifacts: it builds from source",
		},
		Placeholder: true,
		Note: "Not yet written (Task 25). Coverage here is the intended shape, not an " +
			"executed result.",
	},
	{
		Gate: "upgrade-schema-e2e", Runner: RunnerDocker,
		Proves: []string{"baseline-coordinator-on-schema-19"},
		DoesNotProve: []string{
			"donor behaviour: no donor participates",
			"that a DOWN migration works — the contract is restore-from-backup, and " +
				"nothing here exercises a downgrade of the schema itself",
		},
		Placeholder: true,
		Note: "Not yet written (Task 25). This is the ONLY gate that can support a " +
			"rollback-safe claim, and nothing tests it today.",
	},
	{
		Gate: "upgrade-release-e2e", Runner: RunnerTUN,
		Proves: []string{"baseline-deployment-transition"},
		DoesNotProve: []string{
			"multi-coordinator ordering or fencing, which is a Phase 6 concern",
		},
		Placeholder: true,
		Note:        "Not yet written (Task 25).",
	},
	{
		Gate: "mixed-fleet-e2e", Runner: RunnerTUN,
		Proves: []string{"coordinator-upgrade-needs-no-donor-upgrade"},
		DoesNotProve: []string{
			"anything about donors outside the declared support window: an unsupported " +
				"donor is untested by definition, which is what unsupported means",
		},
		Placeholder: true,
		Note:        "Not yet written (Task 26).",
	},
	{
		// DERIVED FROM AN EXECUTED RUN, 2026-08-11, all three pairings against
		// predecessor commit 143c459 read from the release intent.
		Gate: "crossversion-e2e", Runner: RunnerDocker,
		Proves: nil,
		DoesNotProve: []string{
			"any DECLARED claim: baseline-donor-interop is assigned to upgrade-wire-e2e, " +
				"which runs the protocol against a fresh database. This gate overlaps it " +
				"heavily but is not the same scope, and re-pointing a reviewed claim at a " +
				"gate that happens to cover it is how coverage stops meaning anything",
			"anything about the schema: every pairing gets a FRESH database migrated by the " +
				"coordinator side's own binary, so no binary is ever run against a schema it " +
				"did not produce",
			"anything about released artifacts: both sides are built from source",
			"donor-backed reads or drain WITH THE BASELINE COORDINATOR: the gate exercises " +
				"those only when the coordinator is HEAD. Untested here, not known broken — " +
				"the earlier claim that they were provably broken described the P2-M6 binary, " +
				"and 143c459 contains the later TLS fix",
		},
		Note: "Executed 2026-08-11 against 143c459. head×head, head-coordinator×baseline-donor " +
			"and baseline-coordinator×head-donor all passed: registration over federation " +
			"mTLS, assignment, ack, a decided possession audit, and a hash-verified serve. " +
			"The two HEAD-coordinator pairings additionally proved a hash-verified " +
			"DONOR-BACKED read with the coordinator's local Kubo repo wiped, and drain/undrain. " +
			"The P2-M6-era caveats about fabricated audits and broken donor reads are gone " +
			"with the predecessor they described.",
	},
}

// Coverage returns the checked-in gate-coverage table.
func Coverage() []GateCoverage { return slices.Clone(coverage) }

// CoverageFor looks up one gate.
func CoverageFor(gate string) (GateCoverage, bool) {
	for _, c := range coverage {
		if c.Gate == gate {
			return c, true
		}
	}
	return GateCoverage{}, false
}

// ValidateClaimCoverage refuses a claim whose gate does not cover it. Without
// this, "proven_by_gate" is a label rather than a constraint.
func ValidateClaimCoverage(in Intent, table []GateCoverage) error {
	byGate := map[string]GateCoverage{}
	for _, c := range table {
		byGate[c.Gate] = c
	}
	for _, claim := range in.Claims {
		g, ok := byGate[claim.ProvenByGate]
		if !ok {
			return fmt.Errorf("claim %q names gate %q, which has no coverage entry; a gate "+
				"nobody described proves nothing", claim.ID, claim.ProvenByGate)
		}
		if !slices.Contains(g.Proves, claim.ID) {
			return fmt.Errorf("claim %q is beyond gate %q's declared coverage (it proves %v)",
				claim.ID, g.Gate, g.Proves)
		}
	}
	return nil
}

// ValidateScenarios checks that every scenario names a real gate, that its
// claims are declared, and that its artifact identities are well-formed.
func ValidateScenarios(in Intent, scenarios []Scenario, table []GateCoverage) error {
	declared := map[string]bool{}
	for _, c := range in.Claims {
		declared[c.ID] = true
	}
	seen := map[string]bool{}
	for _, s := range scenarios {
		if s.ID == "" {
			return fmt.Errorf("scenario %+v has no id", s)
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate scenario id %q", s.ID)
		}
		seen[s.ID] = true
		for _, side := range []struct{ role, id string }{
			{"coordinator", s.Coordinator}, {"donor", s.Donor},
		} {
			if err := validateArtifactID(side.id); err != nil {
				return fmt.Errorf("scenario %q %s: %w", s.ID, side.role, err)
			}
		}
		if _, ok := CoverageForIn(table, s.Gate); !ok {
			return fmt.Errorf("scenario %q names gate %q, which has no coverage entry", s.ID, s.Gate)
		}
		for _, id := range s.Claims {
			if !declared[id] {
				return fmt.Errorf("scenario %q claims %q, which the intent never declared", s.ID, id)
			}
		}
	}
	return nil
}

// CoverageForIn is CoverageFor against a supplied table, for tests and for the
// release workflow's staged tables.
func CoverageForIn(table []GateCoverage, gate string) (GateCoverage, bool) {
	for _, c := range table {
		if c.Gate == gate {
			return c, true
		}
	}
	return GateCoverage{}, false
}

// validateArtifactID accepts a product version or a commit identity.
//
// The commit form is not a convenience: the baseline has no product version,
// and the transition from it is the one that has to work.
func validateArtifactID(id string) error {
	if commit, ok := strings.CutPrefix(id, "commit:"); ok {
		if len(commit) < 7 || strings.TrimLeft(commit, "0123456789abcdef") != "" {
			return fmt.Errorf("%q is not a hex commit of at least 7 characters", id)
		}
		return nil
	}
	return validateProductVersion(id)
}

// ReleaseCandidateReady reports whether the coverage table can support a
// release candidate.
//
// A placeholder means the coverage is the INTENDED shape rather than an
// executed result. Promoting on that would put a claim in a signed lock with
// nothing behind it, which is precisely the failure the evidence model exists
// to prevent.
func ReleaseCandidateReady(table []GateCoverage) error {
	var pending []string
	for _, c := range table {
		if c.Placeholder {
			pending = append(pending, c.Gate)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("release candidate blocked: gate coverage is still a placeholder for "+
			"%s; a claim bound to intended coverage is a claim with nothing behind it",
			strings.Join(pending, ", "))
	}
	return nil
}
