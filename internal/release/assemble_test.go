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

// descriptorFor is an index publishing exactly the checked-in intent's declared
// platforms, plus the attestation manifest BuildKit adds. An index with no
// children is refused: an index IS its list of manifests, and one recorded
// without them cannot say what it runs on.
func descriptorFor(name string, fill byte) LockedArtifact {
	children := []ocispec.Descriptor{{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.Digest("sha256:" + strings.Repeat(string(fill), 63) + "a"),
		Size:      512,
		Platform:  &ocispec.Platform{OS: "linux", Architecture: "amd64"},
	}, {
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.Digest("sha256:" + strings.Repeat(string(fill), 63) + "b"),
		Size:      64,
		Platform:  &ocispec.Platform{OS: "unknown", Architecture: "unknown"},
	}}
	return LockedArtifact{
		Repository: "ghcr.io/nova-archive/" + name,
		Descriptor: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageIndex,
			Digest:    digest.Digest("sha256:" + strings.Repeat(string(fill), 64)),
			Size:      1024,
		},
		Manifests: children,
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
func passingEvidence(t *testing.T, in Intent, intentBytes []byte, arts map[string]LockedArtifact) map[string]EvidencePair {
	t.Helper()
	digests := map[string]string{}
	for name, a := range arts {
		digests[name] = a.Descriptor.Digest.String()
	}
	out := map[string]EvidencePair{}
	for _, c := range in.Claims {
		cov, ok := CoverageForIn(Coverage(), c.ProvenByGate)
		if !ok {
			t.Fatalf("claim %s names gate %s, which has no coverage entry", c.ID, c.ProvenByGate)
		}
		st := EvidenceStatement{
			Schema: EvidenceSchema, Gate: c.ProvenByGate,
			RunnerClass: cov.Runner, Outcome: "passed",
			Release: in.Version, SourceCommit: "143c4590000",
			IntentDigest:    IntentDigest(intentBytes),
			ArtifactDigests: digests, Claims: []string{c.ID},
			// A claim conditional on capabilities needs evidence that
			// records exercising them; the condition is the part that
			// would otherwise go untested.
			Capabilities: c.RequiresCapabilities,
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

	ev := passingEvidence(t, in, b, testArtifacts())
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
	ev := passingEvidence(t, in, b, arts)
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
	ev := passingEvidence(t, in, b, arts)

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
		cov, ok := CoverageForIn(Coverage(), c.ProvenByGate)
		if !ok || cov.Runner == RunnerStatic {
			continue
		}
		st := EvidenceStatement{
			Schema: EvidenceSchema, Gate: c.ProvenByGate,
			RunnerClass: RunnerStatic, Outcome: "passed",
			Release: in.Version, SourceCommit: "abc",
			IntentDigest:    "sha256:" + strings.Repeat("9", 64),
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
		IntentDigest:    "sha256:" + strings.Repeat("9", 64),
		ArtifactDigests: map[string]string{"nova-node": "sha256:" + strings.Repeat("2", 64)},
		TestRevision:    "abc",
	}
	if err := AssertEvidenceSupportsClaim(c, st, derivedCoverage()); err == nil {
		t.Fatal("a skipped gate must not prove a claim")
	}
}

// TestBuildLockRefusesAPrePublicationPlaceholder.
//
// `ReleaseCandidateReady`'s refusal, enforced a second time at the point it
// matters most: even a release engineer with a perfect bundle, every gate run
// and a statement for every claim cannot produce a lock while a gate a claim
// DEPENDS ON carries intended rather than executed coverage.
//
// A POST-publication placeholder is deliberately not this. That gate cannot run
// before the release exists, so requiring it here would make a first release
// impossible — see TestPostPublicationCoverageDoesNotBlockALock.
func TestBuildLockRefusesAPrePublicationPlaceholder(t *testing.T) {
	b, in := readCommittedIntent(t)
	arts := testArtifacts()

	blocked := Coverage()
	var gate string
	for i := range blocked {
		if !blocked[i].PostPublication && len(blocked[i].Proves) > 0 {
			blocked[i].Placeholder = true
			gate = blocked[i].Gate
			break
		}
	}
	if gate == "" {
		t.Skip("no pre-publication gate proves a claim")
	}

	_, err := BuildLock(LockInputs{
		Intent: in, IntentBytes: b, SourceCommit: "143c4590000", BuiltAt: time.Now(),
		Artifacts: arts, Sidecars: in.Sidecars,
		Evidence: passingEvidence(t, in, b, arts),
		Payload:  map[string]string{MemberIntent: IntentDigest(b)},
		Coverage: blocked,
	})
	if err == nil {
		t.Fatalf("a lock was built while %s carried placeholder coverage", gate)
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("err = %v, want a refusal naming the placeholder", err)
	}
}

// TestPostPublicationCoverageDoesNotBlockALock. The checked-in table has
// exactly this shape today: upgrade-release-e2e is outstanding, and a lock can
// still be cut. Requiring it would require a release to prove something about
// itself before it existed.
func TestPostPublicationCoverageDoesNotBlockALock(t *testing.T) {
	b, in := readCommittedIntent(t)
	arts := testArtifacts()

	var post bool
	for _, c := range Coverage() {
		if c.PostPublication && c.Placeholder {
			post = true
		}
	}
	if !post {
		t.Skip("no outstanding post-publication gate")
	}

	// A REAL bundle and the REAL checked-in coverage. A minimal payload would
	// fail lock validation for an unrelated reason and prove nothing about the
	// question under test.
	ev := passingEvidence(t, in, b, arts)
	evFiles := map[string][]byte{}
	for id, pair := range ev {
		body, err := pair.Statement.Render()
		if err != nil {
			t.Fatal(err)
		}
		evFiles["evidence/"+id+".json"] = body
	}
	payload, err := AssembleBundle(filepath.Join(t.TempDir(), "bundle"), BundleInputs{
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

	if _, err := BuildLock(LockInputs{
		Intent: in, IntentBytes: b, SourceCommit: "143c4590000", BuiltAt: time.Now(),
		Artifacts: arts, Sidecars: in.Sidecars,
		Evidence: ev, Payload: payload, Coverage: Coverage(),
	}); err != nil {
		t.Fatalf("an outstanding post-publication gate blocked the lock: %v", err)
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
		IntentDigest:    "sha256:" + strings.Repeat("9", 64),
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
