package release

import (
	"encoding/json"
	"fmt"
	"sort"
)

// The release plan (P2-M7.3, amendment).
//
// # What the workflow used to do instead
//
// release.yml carried five literal `make` lines. That is wrong in one specific
// way: adding a claim to a LATER release's intent passes every check in this
// package — the claim is declared, the gate has coverage, the coverage proves
// it — and then the gate silently does not run, because nobody edited the YAML.
// The lock build would catch it at the very end, having already pushed and
// signed three images.
//
// So the workflow asks this package what to run. The plan is derived from the
// intent's claims and the coverage table, which are the two documents that
// already know the answer, and a release whose intent names a gate nothing
// executes cannot get past `novarel plan`.
//
// # The split is in the plan, not in the caller
//
// Pre-publication gates produce evidence the lock cites. Post-publication gates
// produce evidence nothing cites — they gate completion state 4. A caller that
// had to remember which was which would eventually run a post-publication gate
// before publication, get a skip, and wonder why the lock refused it.

// PlannedGate is one gate the workflow must execute, with everything the
// resulting evidence statement needs to record.
type PlannedGate struct {
	Gate       string `json:"gate"`
	MakeTarget string `json:"make_target"`
	Runner     string `json:"runner"`
	// Claims are the intent claims this gate proves in THIS release. Empty for
	// a gate that runs for coverage rather than for a claim.
	Claims []string `json:"claims,omitempty"`
	// Capabilities are the capabilities the claims are conditional on.
	Capabilities []string `json:"capabilities,omitempty"`
	// Scenarios are the design document's acceptance scenarios.
	Scenarios []string `json:"scenarios,omitempty"`
	// PostPublication marks a gate that cannot run until the release exists.
	PostPublication bool `json:"post_publication"`
}

// ReleasePlan is everything the release workflow needs that is derivable from
// the reviewed intent.
type ReleasePlan struct {
	Version      string            `json:"version"`
	IntentPath   string            `json:"intent_path"`
	IntentDigest string            `json:"intent_digest"`
	TargetSchema int               `json:"target_schema"`
	Platforms    []string          `json:"platforms"`
	Repositories map[string]string `json:"repositories"`
	// Predecessors are the artifact identities this release commits to
	// interoperating with, newest first — the git refs a cross-version or
	// mixed-fleet gate checks out.
	Predecessors []string `json:"predecessors"`
	// PrePublication gates run against the candidate digests and produce the
	// evidence the lock cites.
	PrePublication []PlannedGate `json:"pre_publication_gates"`
	// PostPublication gates run against the published release and cite
	// nothing; they gate completion state 4.
	PostPublication []PlannedGate `json:"post_publication_gates"`
}

// BuildPlan derives the plan, refusing anything that would produce a lock the
// workflow could not finish.
func BuildPlan(in Intent, intentBytes []byte, path string, table []GateCoverage) (ReleasePlan, error) {
	// Coverage first. A claim bound to a gate that cannot prove it, or to a
	// post-publication gate, is a release that fails after three images have
	// been pushed and signed rather than before anything was built.
	if err := ValidateClaimCoverage(in, table); err != nil {
		return ReleasePlan{}, err
	}
	if err := ReleaseCandidateReady(table); err != nil {
		return ReleasePlan{}, err
	}

	claimsByGate := map[string][]string{}
	capsByGate := map[string][]string{}
	for _, c := range in.Claims {
		claimsByGate[c.ProvenByGate] = append(claimsByGate[c.ProvenByGate], c.ID)
		capsByGate[c.ProvenByGate] = append(capsByGate[c.ProvenByGate], c.RequiresCapabilities...)
	}

	p := ReleasePlan{
		Version:      in.Version,
		IntentPath:   path,
		IntentDigest: IntentDigest(intentBytes),
		TargetSchema: in.TargetSchema,
		Platforms:    in.Platforms,
		Repositories: in.Repositories,
	}
	for _, pred := range in.SupportedPredecessors {
		p.Predecessors = append(p.Predecessors, pred.ID())
	}

	for _, cov := range table {
		claims := claimsByGate[cov.Gate]
		if len(claims) == 0 && !cov.PostPublication && !cov.RequiredForRelease {
			// A gate no claim in THIS release depends on is not the release
			// workflow's business. It still runs in CI; running it here would
			// spend an hour proving something no document cites.
			continue
		}
		if cov.MakeTarget == "" {
			return ReleasePlan{}, fmt.Errorf("plan: gate %q has no make target, so the workflow "+
				"has no way to execute it", cov.Gate)
		}
		sort.Strings(claims)
		g := PlannedGate{
			Gate: cov.Gate, MakeTarget: cov.MakeTarget, Runner: string(cov.Runner),
			Claims:          claims,
			Capabilities:    dedupeSorted(capsByGate[cov.Gate]),
			Scenarios:       cov.AcceptanceScenarios,
			PostPublication: cov.PostPublication,
		}
		if cov.PostPublication {
			p.PostPublication = append(p.PostPublication, g)
		} else {
			p.PrePublication = append(p.PrePublication, g)
		}
	}

	if len(p.PrePublication) == 0 {
		return ReleasePlan{}, fmt.Errorf("plan: %s declares %d claim(s) and the plan runs no "+
			"pre-publication gate; a release whose evidence job does nothing would produce a "+
			"lock citing statements nothing wrote", in.Version, len(in.Claims))
	}
	if len(p.PostPublication) == 0 {
		return ReleasePlan{}, fmt.Errorf("plan: no post-publication gate. Completion state 4 " +
			"would be unreachable and every release would be permanently 🟡; if that is really " +
			"intended, it has to be a decision in the coverage table rather than an omission")
	}
	return p, nil
}

// Render writes the plan as the workflow reads it.
func (p ReleasePlan) Render() ([]byte, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
