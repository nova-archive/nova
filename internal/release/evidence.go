package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

// Signed evidence statements (P2-M7.3, D-M7.3-2b).
//
// # The gap this closes
//
// `EvidenceRef` referenced a statement by content digest, and nothing produced
// a statement. So the lock could reference evidence that had never been
// written, and the release workflow's "emit signed evidence statements" step
// was the point at which it stopped. A reference to a document that does not
// exist is not evidence; it is a citation.
//
// # Why a statement and not a CI run id
//
// Logs expire. Runs are deleted. A run id proves nothing about what the run
// concluded — anyone reading it later has to trust that somebody looked. A
// statement carries the conclusion itself, plus everything needed to decide
// whether it is about the artifacts in front of you: the exact artifact
// digests, the source commit, the scenario and capability ids, the test
// revision, the outcome, the timestamps and the runner class.
//
// # The runner class is load-bearing
//
// A static gate may never satisfy a claim that needs an executed one
// (D-M7.3-17). The class is recorded ON the statement rather than looked up
// from the gate name, so a gate that is later moved to a cheaper tier cannot
// retroactively change what its old statements mean.

// EvidenceSchema is the statement format's own version.
const EvidenceSchema = 1

// EvidenceStatement is one gate's conclusion about one set of artifacts.
type EvidenceStatement struct {
	Schema int    `json:"schema"`
	Gate   string `json:"gate"`
	// RunnerClass is the tier the gate actually ran in.
	RunnerClass RunnerClass `json:"runner_class"`
	// Outcome is "passed" or "skipped". There is no "failed": a failed gate
	// produces no statement, because a statement is what a claim cites.
	Outcome string `json:"outcome"`

	// Release and SourceCommit say which candidate this is about.
	Release      string `json:"release"`
	SourceCommit string `json:"source_commit"`
	// ArtifactDigests maps artifact name to the exact digest under test. A
	// statement that does not name the bytes it examined can be reused for a
	// different build, which is the whole failure mode.
	ArtifactDigests map[string]string `json:"artifact_digests"`

	// Claims are the claim ids this statement supports.
	Claims []string `json:"claims,omitempty"`
	// Scenarios and Capabilities record what was exercised.
	Scenarios    []string `json:"scenarios,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`

	// TestRevision is the commit of the test code that ran, which can differ
	// from SourceCommit when a gate is run against an older candidate.
	TestRevision string `json:"test_revision"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// Detail is free-form context — a skip's reason, a count, a log excerpt.
	Detail string `json:"detail,omitempty"`
}

// Render writes the statement in the canonical byte form its digest is taken
// over. Pinned here so a caller cannot change the digest by passing different
// marshalling options.
func (e EvidenceStatement) Render() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Validate refuses a statement that cannot be acted on.
func (e EvidenceStatement) Validate() error {
	if e.Schema != EvidenceSchema {
		return fmt.Errorf("evidence: schema %d, want %d", e.Schema, EvidenceSchema)
	}
	if e.Gate == "" {
		return errors.New("evidence: no gate; a conclusion with no source is an assertion")
	}
	switch e.Outcome {
	case "passed", "skipped":
	default:
		return fmt.Errorf("evidence: outcome %q; a statement records passed or skipped, and a "+
			"FAILED gate produces no statement at all — there is nothing for a claim to cite",
			e.Outcome)
	}
	switch e.RunnerClass {
	case RunnerStatic, RunnerDocker, RunnerTUN:
	default:
		return fmt.Errorf("evidence: runner class %q is not a declared tier", e.RunnerClass)
	}
	if e.Release == "" || e.SourceCommit == "" {
		return errors.New("evidence: a statement must name the release and the source commit " +
			"it is about")
	}
	if len(e.ArtifactDigests) == 0 {
		return errors.New("evidence: no artifact digests. A statement that does not name the " +
			"bytes it examined can be reused for a different build")
	}
	for name, d := range e.ArtifactDigests {
		if len(d) != 71 || d[:7] != "sha256:" {
			return fmt.Errorf("evidence: %s digest %q is not a sha256 digest", name, d)
		}
	}
	if e.TestRevision == "" {
		return errors.New("evidence: no test revision; a conclusion is about a version of the " +
			"test as much as a version of the code")
	}
	if e.FinishedAt.Before(e.StartedAt) {
		return errors.New("evidence: finished before it started")
	}
	if e.Outcome == "skipped" && e.Detail == "" {
		return errors.New("evidence: a skip with no reason is indistinguishable from a pass")
	}
	return nil
}

// EvidenceDigest is how the lock references a statement: sha256 over the exact
// rendered bytes, the same rule as everything else here.
func EvidenceDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParseEvidence reads a statement back and validates it.
func ParseEvidence(b []byte) (EvidenceStatement, error) {
	var e EvidenceStatement
	if err := json.Unmarshal(b, &e); err != nil {
		return EvidenceStatement{}, fmt.Errorf("evidence: does not parse: %w", err)
	}
	if err := e.Validate(); err != nil {
		return EvidenceStatement{}, err
	}
	return e, nil
}

// Ref converts a statement into the reference the lock stores.
func (e EvidenceStatement) Ref(digest string) EvidenceRef {
	return EvidenceRef{
		StatementDigest: digest,
		Gate:            e.Gate,
		Outcome:         e.Outcome,
		RunnerClass:     string(e.RunnerClass),
		At:              e.FinishedAt,
	}
}

// AssertEvidenceSupportsClaim is the check that stops a claim being proven by
// something that cannot prove it.
//
// Three ways it goes wrong, all of which have to be refused rather than warned
// about, because the result of getting this wrong is a signed document that
// says something untrue:
//
//   - the statement is from a DIFFERENT gate than the claim declared;
//   - the statement's outcome is `skipped`, so nothing was demonstrated;
//   - the statement ran in a cheaper tier than the gate's coverage declares,
//     which is a static check standing in for a drill.
func AssertEvidenceSupportsClaim(c DeclaredClaim, e EvidenceStatement, table []GateCoverage) error {
	if e.Gate != c.ProvenByGate {
		return fmt.Errorf("evidence: claim %q declares gate %q but the statement is from %q",
			c.ID, c.ProvenByGate, e.Gate)
	}
	if e.Outcome != "passed" {
		return fmt.Errorf("evidence: claim %q cannot be proven by a %s gate; nothing was "+
			"demonstrated", c.ID, e.Outcome)
	}
	cov, ok := CoverageForIn(table, c.ProvenByGate)
	if !ok {
		return fmt.Errorf("evidence: gate %q has no coverage entry, so there is nothing that "+
			"says what it can prove", c.ProvenByGate)
	}
	if cov.PostPublication {
		return fmt.Errorf("evidence: gate %q runs AFTER publication, so it cannot prove claim "+
			"%q — the lock that would carry the claim is signed before this gate can run. "+
			"A post-publication result belongs to completion state 4, not to a lock",
			c.ProvenByGate, c.ID)
	}
	if cov.Placeholder {
		return fmt.Errorf("evidence: gate %q's coverage is still a placeholder — the intended "+
			"shape rather than an executed result — so claim %q has nothing behind it",
			c.ProvenByGate, c.ID)
	}
	if !slices.Contains(cov.Proves, c.ID) {
		return fmt.Errorf("evidence: gate %q's coverage does not list %q among what it proves",
			c.ProvenByGate, c.ID)
	}
	if tierRank(e.RunnerClass) < tierRank(cov.Runner) {
		return fmt.Errorf("evidence: claim %q needs a %s gate but the statement ran in %s; a "+
			"cheaper tier may never stand in for a more capable one",
			c.ID, cov.Runner, e.RunnerClass)
	}
	return nil
}

// tierRank orders the tiers by capability. Static cannot substitute for Docker,
// and Docker cannot substitute for TUN.
func tierRank(r RunnerClass) int {
	switch r {
	case RunnerStatic:
		return 1
	case RunnerDocker:
		return 2
	case RunnerTUN:
		return 3
	}
	return 0
}

// SortedEvidencePaths returns the bundle-relative paths for a set of
// statements, in a stable order.
func SortedEvidencePaths(files map[string][]byte) []string {
	out := make([]string, 0, len(files))
	for p := range files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
