package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The release path, end to end (P2-M7.3 amendments).
//
// Before these, the package could parse a lock, verify a bundle and reference
// an evidence statement — and could produce none of the three. Every lock that
// existed was a test fixture, which is why the release workflow stopped at
// "finalize and sign the lock": there was nothing to call.

func descriptorFor(name string, fill byte) LockedArtifact {
	return LockedArtifact{
		Repository: "ghcr.io/nova-archive/" + name,
		Descriptor: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageIndex,
			Digest:    digest.Digest("sha256:" + strings.Repeat(string(fill), 64)),
			Size:      1024,
		},
	}
}

func testArtifacts() map[string]LockedArtifact {
	return map[string]LockedArtifact{
		"nova-coordinator": descriptorFor("nova-coordinator", '1'),
		"nova-node":        descriptorFor("nova-node", '2'),
		"nova-admin":       descriptorFor("nova-admin", '3'),
	}
}

// derivedCoverage is the checked-in table with every placeholder retired.
//
// It models the state a real release candidate must reach. The MECHANISM is
// tested against it; that the checked-in table refuses TODAY is asserted
// separately, in TestBuildLockRefusesWhileCoverageIsAPlaceholder.
func derivedCoverage() []GateCoverage {
	out := Coverage()
	for i := range out {
		out[i].Placeholder = false
	}
	return out
}

// passingEvidence builds a statement that genuinely supports every claim.
func passingEvidence(t *testing.T, in Intent, arts map[string]LockedArtifact) map[string]EvidencePair {
	t.Helper()
	digests := map[string]string{}
	for name, a := range arts {
		digests[name] = a.Descriptor.Digest.String()
	}
	out := map[string]EvidencePair{}
	for _, c := range in.Claims {
		cov, ok := CoverageForIn(derivedCoverage(), c.ProvenByGate)
		if !ok {
			t.Fatalf("claim %s names gate %s, which has no coverage entry", c.ID, c.ProvenByGate)
		}
		st := EvidenceStatement{
			Schema: EvidenceSchema, Gate: c.ProvenByGate,
			RunnerClass: cov.Runner, Outcome: "passed",
			Release: in.Version, SourceCommit: "143c4590000",
			ArtifactDigests: digests, Claims: []string{c.ID},
			TestRevision: "143c4590000",
			StartedAt:    time.Now().UTC().Add(-time.Minute), FinishedAt: time.Now().UTC(),
		}
		body, err := st.Render()
		if err != nil {
			t.Fatal(err)
		}
		out[c.ID] = EvidencePair{Statement: st, Digest: EvidenceDigest(body)}
	}
	return out
}

// TestAssembleBuildAndVerifyRoundTrip. Assemble a bundle, build a lock over it,
// and verify the bundle against that lock the way an operator's bootstrap will.
func TestAssembleBuildAndVerifyRoundTrip(t *testing.T) {
	b, in := readCommittedIntent(t)
	dir := filepath.Join(t.TempDir(), "bundle")

	ev := passingEvidence(t, in, testArtifacts())
	evFiles := map[string][]byte{}
	for id, pair := range ev {
		body, err := pair.Statement.Render()
		if err != nil {
			t.Fatal(err)
		}
		evFiles["evidence/"+id+".json"] = body
	}

	payload, err := AssembleBundle(dir, BundleInputs{
		IntentBytes: b,
		Policy:      []byte("issuer=x\nidentity=y\n"),
		ComposeEnv:  []byte("NOVA_RELEASE_VERSION=" + in.Version + "\n"),
		Upgrading:   []byte("# Upgrading\n"),
		Bootstrap:   []byte("#!/usr/bin/env bash\nexit 0\n"),
		Evidence:    evFiles,
	})
	if err != nil {
		t.Fatal(err)
	}

	// hashes.txt is DERIVED from the map and then covered BY it. Both halves
	// matter: derived so it cannot disagree with the authority, covered so it
	// is not an unsigned file riding along.
	if _, ok := payload[MemberHashes]; !ok {
		t.Error("hashes.txt is not covered by the payload map")
	}
	if _, ok := payload["lock.json"]; ok {
		t.Error("the payload map covers the lock, which is the cycle the design forbids")
	}

	lock, err := BuildLock(LockInputs{
		Intent: in, IntentBytes: b,
		SourceCommit: "143c4590000", BuiltAt: time.Now().UTC(),
		Artifacts: testArtifacts(), Sidecars: in.Sidecars,
		Evidence: ev, Payload: payload, Coverage: derivedCoverage(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lock.DonorLockDigest == "" {
		t.Error("the lock does not carry the donor-lock projection's digest")
	}

	body, err := RenderLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lock.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	// The bundle verifies against the lock built for it.
	if err := VerifyAssembled(dir, lock); err != nil {
		t.Fatalf("the assembled bundle does not verify against its own lock: %v", err)
	}

	// And the lock parses through the same path a consumer uses. A builder with
	// its own notion of validity emits documents nothing accepts.
	if _, err := ParseLock(body, in, b); err != nil {
		t.Fatalf("the lock this package built does not parse: %v", err)
	}

	// The bootstrap must be executable where it lands.
	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(MemberBootstrap)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("scripts/nova-release is not executable in the assembled bundle")
	}
}

// TestBuildLockRefusesAClaimWithNoEvidence. This is the failure the whole
// intent/lock split exists to prevent.
func TestBuildLockRefusesAClaimWithNoEvidence(t *testing.T) {
	b, in := readCommittedIntent(t)
	arts := testArtifacts()
	ev := passingEvidence(t, in, arts)
	for id := range ev {
		delete(ev, id)
		break
	}

	_, err := BuildLock(LockInputs{
		Intent: in, IntentBytes: b, SourceCommit: "abc", BuiltAt: time.Now(),
		Artifacts: arts, Sidecars: in.Sidecars, Evidence: ev,
		Payload:  map[string]string{MemberIntent: IntentDigest(b)},
		Coverage: derivedCoverage(),
	})
	if err == nil || !strings.Contains(err.Error(), "no evidence statement") {
		t.Fatalf("err = %v, want a refusal naming the unevidenced claim", err)
	}
}

// TestBuildLockRefusesEvidenceAboutDifferentArtifacts. A passing run against
// last week's build must not prove this week's.
func TestBuildLockRefusesEvidenceAboutDifferentArtifacts(t *testing.T) {
	b, in := readCommittedIntent(t)
	arts := testArtifacts()
	ev := passingEvidence(t, in, arts)

	// Promote something else.
	other := testArtifacts()
	other["nova-node"] = descriptorFor("nova-node", '9')

	_, err := BuildLock(LockInputs{
		Intent: in, IntentBytes: b, SourceCommit: "abc", BuiltAt: time.Now(),
		Artifacts: other, Sidecars: in.Sidecars, Evidence: ev,
		Payload:  map[string]string{MemberIntent: IntentDigest(b)},
		Coverage: derivedCoverage(),
	})
	if err == nil || !strings.Contains(err.Error(), "evidence examined") {
		t.Fatalf("err = %v, want a refusal naming the artifact the evidence was about", err)
	}
}

// TestEvidenceCannotComeFromACheaperTier. A static gate may never satisfy a
// claim that needs an executed one, and the runner class is recorded on the
// statement so moving a gate later cannot change what its old statements mean.
func TestEvidenceCannotComeFromACheaperTier(t *testing.T) {
	_, in := readCommittedIntent(t)
	for _, c := range in.Claims {
		cov, ok := CoverageForIn(derivedCoverage(), c.ProvenByGate)
		if !ok || cov.Runner == RunnerStatic {
			continue
		}
		st := EvidenceStatement{
			Schema: EvidenceSchema, Gate: c.ProvenByGate,
			RunnerClass: RunnerStatic, Outcome: "passed",
			Release: in.Version, SourceCommit: "abc",
			ArtifactDigests: map[string]string{"nova-node": "sha256:" + strings.Repeat("2", 64)},
			TestRevision:    "abc",
		}
		err := AssertEvidenceSupportsClaim(c, st, derivedCoverage())
		if err == nil || !strings.Contains(err.Error(), "cheaper tier") {
			t.Errorf("claim %s: err = %v, want a tier refusal", c.ID, err)
		}
		return
	}
	t.Skip("no claim currently needs a tier above static")
}

// TestEvidenceCannotBeSkipped. A claim proven by a skipped gate is a claim with
// nothing behind it.
func TestEvidenceCannotBeSkipped(t *testing.T) {
	_, in := readCommittedIntent(t)
	c := in.Claims[0]
	cov, _ := CoverageForIn(derivedCoverage(), c.ProvenByGate)
	st := EvidenceStatement{
		Schema: EvidenceSchema, Gate: c.ProvenByGate, RunnerClass: cov.Runner,
		Outcome: "skipped", Detail: "no published artifacts",
		Release: in.Version, SourceCommit: "abc",
		ArtifactDigests: map[string]string{"nova-node": "sha256:" + strings.Repeat("2", 64)},
		TestRevision:    "abc",
	}
	if err := AssertEvidenceSupportsClaim(c, st, derivedCoverage()); err == nil {
		t.Fatal("a skipped gate must not prove a claim")
	}
}

// TestBuildLockRefusesWhileCoverageIsAPlaceholder.
//
// This is `ReleaseCandidateReady`'s refusal enforced a second time, at the
// point it matters most: even a release engineer who assembled a perfect
// bundle, ran every gate that CAN run, and emitted statements for every claim
// cannot produce a lock while a gate's coverage is the intended shape rather
// than an executed result.
//
// Two of them are today — upgrade-release-e2e and mixed-fleet-e2e — so this
// test asserts against the REAL checked-in table, and will start failing the
// day both are written. That is the right time to be told.
func TestBuildLockRefusesWhileCoverageIsAPlaceholder(t *testing.T) {
	b, in := readCommittedIntent(t)
	arts := testArtifacts()

	var placeholders []string
	for _, c := range Coverage() {
		if c.Placeholder {
			placeholders = append(placeholders, c.Gate)
		}
	}
	if len(placeholders) == 0 {
		t.Skip("every gate's coverage is derived; this milestone's remaining gap is closed")
	}

	_, err := BuildLock(LockInputs{
		Intent: in, IntentBytes: b, SourceCommit: "abc", BuiltAt: time.Now(),
		Artifacts: arts, Sidecars: in.Sidecars,
		Evidence: passingEvidence(t, in, arts),
		Payload:  map[string]string{MemberIntent: IntentDigest(b)},
		Coverage: Coverage(),
	})
	if err == nil {
		t.Fatalf("a lock was built while %v still carry placeholder coverage", placeholders)
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("err = %v, want a refusal naming the placeholder", err)
	}
}

// TestEvidenceRejectsAFailedOutcome. There is no "failed" statement: a failed
// gate produces nothing, because a statement is what a claim cites.
func TestEvidenceRejectsAFailedOutcome(t *testing.T) {
	st := EvidenceStatement{
		Schema: EvidenceSchema, Gate: "g", RunnerClass: RunnerDocker, Outcome: "failed",
		Release: "v0.3.0", SourceCommit: "abc",
		ArtifactDigests: map[string]string{"nova-node": "sha256:" + strings.Repeat("2", 64)},
		TestRevision:    "abc",
	}
	if _, err := st.Render(); err == nil {
		t.Fatal("a failed outcome must not render as a statement")
	}
	st.Outcome = "skipped"
	if _, err := st.Render(); err == nil {
		t.Fatal("a skip with no reason is indistinguishable from a pass")
	}
}

// TestAssembleRefusesABundleWithNoEvidence. Every claim in the lock cites a
// statement; a bundle that omits them ships citations the operator cannot read.
func TestAssembleRefusesABundleWithNoEvidence(t *testing.T) {
	b, _ := readCommittedIntent(t)
	_, err := AssembleBundle(t.TempDir(), BundleInputs{
		IntentBytes: b, Policy: []byte("x"), ComposeEnv: []byte("x"),
		Upgrading: []byte("x"), Bootstrap: []byte("x"),
	})
	if err == nil || !strings.Contains(err.Error(), "no evidence statements") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// TestEvidenceStatementRoundTrips through its rendered bytes, since the digest
// the lock stores is over exactly those.
func TestEvidenceStatementRoundTrips(t *testing.T) {
	st := EvidenceStatement{
		Schema: EvidenceSchema, Gate: "upgrade-schema-e2e", RunnerClass: RunnerDocker,
		Outcome: "passed", Release: "v0.3.0", SourceCommit: "143c459",
		ArtifactDigests: map[string]string{"nova-coordinator": "sha256:" + strings.Repeat("1", 64)},
		Claims:          []string{"baseline-coordinator-on-schema-19"},
		TestRevision:    "143c459",
		StartedAt:       time.Now().UTC().Truncate(time.Second),
		FinishedAt:      time.Now().UTC().Truncate(time.Second),
	}
	body, err := st.Render()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseEvidence(body)
	if err != nil {
		t.Fatal(err)
	}
	again, err := back.Render()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(body) {
		t.Error("a statement does not survive a round trip byte-identically, so its digest is " +
			"not a stable reference")
	}
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatal(err)
	}
	if probe["runner_class"] != string(RunnerDocker) {
		t.Errorf("runner_class = %v", probe["runner_class"])
	}
}
