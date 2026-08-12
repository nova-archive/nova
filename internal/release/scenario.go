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

	// PostPublication marks a gate that CANNOT run before the release exists.
	//
	// Such a gate may never prove an intent claim, and never blocks a lock —
	// requiring it to would be requiring a release to prove something about
	// itself before it existed. It gates COMPLETION instead: see
	// CompletionReady, and the four-state model in docs/ROADMAP.md.
	//
	// The transition to a published release is the case this exists for. It
	// splits in two: the pre-publication half runs against the exact candidate
	// digests and IS provable at lock time; the published half verifies the
	// registry refs, the release assets, the lock as downloaded, and the
	// documented operator path — none of which exist yet when the lock is cut.
	PostPublication bool
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
		// DERIVED FROM AN EXECUTED RUN, 2026-08-11.
		Gate: "upgrade-wire-e2e", Runner: RunnerDocker,
		Proves: []string{"baseline-donor-interop"},
		DoesNotProve: []string{
			"anything about the schema: this gate runs against a FRESH database, so it " +
				"cannot say whether an old binary survives a forward schema",
			"anything about released artifacts: it builds from source",
			"the reverse direction. A baseline COORDINATOR serving a candidate donor is a " +
				"different question and belongs to mixed-fleet-e2e",
		},
		Note: "Executed 2026-08-11 by scripts/upgrade_wire_e2e.sh. A donor built at commit " +
			"143c459 registered over federation mTLS with the candidate coordinator, reached " +
			"active/sync-current, accepted and acknowledged a pin assignment, answered a " +
			"possession audit, and served a hash-verified DONOR-BACKED read with the " +
			"coordinator's local Kubo repo wiped. The script is a thin, purpose-named wrapper " +
			"over the cross-version harness rather than a second implementation: what was " +
			"missing was a named gate a claim could reference, not another way to stand the " +
			"pairing up.",
	},
	{
		// DERIVED FROM AN EXECUTED RUN, 2026-08-11.
		Gate: "upgrade-schema-e2e", Runner: RunnerDocker,
		Proves: []string{"baseline-coordinator-on-schema-19"},
		DoesNotProve: []string{
			"donor behaviour: no donor participates",
			"that a DOWN migration works — the contract is restore-from-backup, and " +
				"nothing here exercises a downgrade of the schema itself",
			"anything about released artifacts: both coordinators are built from source",
			"any range other than the one it ran. The result is about (18, 19], not about " +
				"old-binary compatibility in general",
		},
		Note: "Executed 2026-08-11 by scripts/upgrade_schema_e2e.sh. The database is taken to " +
			"schema 18 by the PREDECESSOR's own migrate, advanced to 19 by the candidate's, " +
			"and the predecessor coordinator (commit 143c459) is then started against it: it " +
			"comes up and answers a users-table query path with 401, which is the query layer " +
			"working rather than merely a process starting. A control run proves the same " +
			"database still serves the candidate, so the result is about the predecessor. " +
			"This is the only gate that can support a rollback-safe claim, and it now does.",
	},
	{
		// PRE-publication. Provable at lock time, because the candidate digests
		// exist by then even though the release does not.
		Gate: "upgrade-candidate-e2e", Runner: RunnerDocker,
		Proves: []string{"candidate-baseline-transition"},
		DoesNotProve: []string{
			"anything about a PUBLISHED release: no registry ref is resolved, no release " +
				"asset is downloaded, and no signature is verified against a real bundle. " +
				"That is upgrade-release-e2e, and it cannot run until the release exists",
			"multi-coordinator ordering or fencing, which is a Phase 6 concern",
		},
		Note: "Executed 2026-08-12 by scripts/upgrade_candidate_e2e.sh. A baseline deployment " +
			"at commit 143c459 and schema 18, carrying a registered donor and archive state, " +
			"crosses to the candidate artifacts by the documented operator path: preflight, " +
			"target-bounded apply, restart, verify. Schema, donor registration and archive " +
			"rows all survive, and the candidate refuses to serve until the migration has run.",
	},
	{
		// POST-publication. It is not bound to an intent claim and never will
		// be: a claim it proved would have to be added to a lock that was
		// already signed, and the v0.3.0 lock is not re-cut.
		Gate: "upgrade-release-e2e", Runner: RunnerTUN,
		Proves: nil,
		DoesNotProve: []string{
			"anything at lock time. It runs AFTER publication, so nothing it concludes can " +
				"appear in the lock it verifies — that document is already signed",
			"multi-coordinator ordering or fencing, which is a Phase 6 concern",
		},
		Placeholder:     true,
		PostPublication: true,
		Note: "MANDATORY FOR COMPLETION STATE 4, and deliberately outside the lock. It verifies " +
			"the published half of the transition: the final registry refs resolve to the " +
			"digests the lock names, the release assets download and verify, the lock as " +
			"DOWNLOADED authenticates, and an operator following docs/UPGRADING.md crosses " +
			"from the baseline to that release. None of it can be true before the release " +
			"exists, which is why the pre-publication half is a separate gate and claim. " +
			"P2-M7.3 is not complete until this passes.",
	},
	{
		// DERIVED FROM AN EXECUTED RUN, 2026-08-12.
		//
		// RunnerDocker, not RunnerTUN. The plan filed this on the TUN tier and
		// it does not belong there: the overlay is transport and this gate is
		// about coordinator policy, so it runs over loopback exactly as the
		// cross-version harness does. That is a promotion in usefulness — a
		// gate that runs in CI beats one waiting on a self-hosted runner — and
		// the limits it buys are recorded below rather than glossed.
		Gate: "mixed-fleet-e2e", Runner: RunnerDocker,
		Proves: []string{"coordinator-upgrade-needs-no-donor-upgrade"},
		DoesNotProve: []string{
			"anything about donors outside the declared support window: an unsupported " +
				"donor is untested by definition, which is what unsupported means",
			"real Nebula routing, MTU behaviour, NAT traversal or lighthouse failure. The " +
				"fleet speaks federation mTLS over loopback with placeholder overlay " +
				"material; federation-deploy-e2e owns the overlay and needs TUN",
			"that a donor FETCHES bytes after an assignment. The gate asserts the " +
				"coordinator's decisions — who is assignable, who is evicted, who holds a " +
				"role — not the transfer, which upgrade-wire-e2e covers",
		},
		Note: "Executed 2026-08-12 by scripts/mixed_fleet_e2e.sh, sixteen assertions, all " +
			"passing. Six donors register SIMULTANEOUSLY against the PREDECESSOR coordinator " +
			"at schema 18 — current, supported-older, capability-missing, pre-contract, " +
			"unsupported-but-compatible and stale — the schema advances to 19, and the " +
			"candidate coordinator takes over. Nobody is evicted, nobody is drained, no " +
			"assignment fails, every version label gates nothing, the capability-missing " +
			"donor loses exactly its role, legacy omission stays distinguishable from " +
			"rollback, and an evicted donor recovers with no re-enrollment. " +
			"The run FOUND A PRODUCTION BUG no single-donor test could: donors sent no " +
			"nebula_cert_fingerprint, the column is UNIQUE NOT NULL, and the second donor " +
			"ever to register got a 500 — a federation with two volunteers could not form.",
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
	for _, c := range in.Claims {
		if cov, ok := CoverageForIn(table, c.ProvenByGate); ok && cov.PostPublication {
			return fmt.Errorf("intent: claim %q is bound to %q, which runs after publication. "+
				"A lock cannot carry it: the lock is signed first. Split the claim — the "+
				"pre-publication half against the candidate digests, the published half as a "+
				"completion gate", c.ID, c.ProvenByGate)
		}
	}

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
		// A post-publication gate is skipped HERE and required by
		// CompletionReady. Demanding it at lock time would demand that a
		// release prove something about itself before it existed — the
		// bootstrap the pre/post split exists to break.
		if c.Placeholder && !c.PostPublication {
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

// CompletionReady reports whether the POST-PUBLICATION evidence exists.
//
// This is completion state 4, and it is a different question from whether a
// release could be cut. A release can ship with this outstanding — that is what
// states 2 and 3 are — but the milestone is not done, and the ROADMAP row does
// not become ✅, until every post-publication gate has run.
//
// Its result is never folded back into the lock. That document was signed
// before this could run, and re-cutting it to add evidence would make the
// signature cover a different set of claims than the one that was reviewed.
func CompletionReady(table []GateCoverage) error {
	var pending []string
	for _, c := range table {
		if c.PostPublication && c.Placeholder {
			pending = append(pending, c.Gate)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("completion state 4 not reached: %s has not run. The release may "+
			"exist; the transition to it has not been demonstrated",
			strings.Join(pending, ", "))
	}
	return nil
}
