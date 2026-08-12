package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The release plan, the platform contract, and the second-release fixture
// (P2-M7.3, amendment).
//
// Everything here answers one question: would the release workflow still be
// correct for a release that is not v0.3.0? The pipeline is dispatched for an
// exact version, reads an exact intent, and derives its gate list from that
// intent's claims — and every one of those is a place where "works for the
// first release" and "works" look identical until the second one.

// TestEveryGateHasAnExecutableMakeTarget.
//
// The workflow runs `make $target` for each gate the plan schedules. A coverage
// entry naming a target that does not exist would pass every check in this
// package and fail in CI, twenty minutes into a release, after three images had
// been pushed and signed.
func TestEveryGateHasAnExecutableMakeTarget(t *testing.T) {
	mk, err := os.ReadFile(makefilePath(t))
	if err != nil {
		t.Fatal(err)
	}
	text := string(mk)
	for _, c := range Coverage() {
		if c.MakeTarget == "" {
			t.Errorf("gate %q has no make target, so the workflow cannot execute it", c.Gate)
			continue
		}
		if !strings.Contains(text, "\n"+c.MakeTarget+":") {
			t.Errorf("gate %q names make target %q, which the Makefile does not define",
				c.Gate, c.MakeTarget)
		}
	}
}

func makefilePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "Makefile")
}

// TestPlanSchedulesTheGateForEveryClaim. The whole reason the plan exists: a
// claim whose gate never runs is a claim with nothing behind it, discovered at
// the lock rather than before the build.
func TestPlanSchedulesTheGateForEveryClaim(t *testing.T) {
	b, in := readCommittedIntent(t)
	p, err := BuildPlan(in, b, "releases/intent/"+in.Version+".json", Coverage())
	if err != nil {
		t.Fatal(err)
	}

	scheduled := map[string]bool{}
	for _, g := range p.PrePublication {
		scheduled[g.Gate] = true
	}
	for _, c := range in.Claims {
		if !scheduled[c.ProvenByGate] {
			t.Errorf("claim %q is proven by %q, which the plan never schedules",
				c.ID, c.ProvenByGate)
		}
	}
	if len(p.PostPublication) == 0 {
		t.Error("no post-publication gate: completion state 4 would be unreachable and every " +
			"release would be permanently incomplete")
	}
	for _, g := range p.PostPublication {
		if len(g.Claims) != 0 {
			t.Errorf("post-publication gate %q carries claims %v; it runs after the lock is "+
				"signed, so nothing it concludes can appear in one", g.Gate, g.Claims)
		}
	}
	if p.IntentDigest != IntentDigest(b) {
		t.Errorf("plan intent digest %s, want %s", p.IntentDigest, IntentDigest(b))
	}
}

// TestPlanCarriesTheCapabilitiesAClaimIsConditionalOn. The evidence statement
// gets these from the plan, and a claim's condition is the part that would
// otherwise go unrecorded.
func TestPlanCarriesTheCapabilitiesAClaimIsConditionalOn(t *testing.T) {
	b, in := readCommittedIntent(t)
	p, err := BuildPlan(in, b, "x.json", Coverage())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range in.Claims {
		if len(c.RequiresCapabilities) == 0 {
			continue
		}
		found := false
		for _, g := range p.PrePublication {
			if g.Gate != c.ProvenByGate {
				continue
			}
			found = true
			for _, want := range c.RequiresCapabilities {
				if !contains(g.Capabilities, want) {
					t.Errorf("gate %q proves %q but the plan does not carry its required "+
						"capability %q", g.Gate, c.ID, want)
				}
			}
		}
		if !found {
			t.Errorf("no planned gate for conditional claim %q", c.ID)
		}
	}
}

// TestPlanRefusesAClaimBoundToAPostPublicationGate. The bootstrap the pre/post
// split exists to break: a lock cannot carry a claim proven after it is signed.
func TestPlanRefusesAClaimBoundToAPostPublicationGate(t *testing.T) {
	b, in := readCommittedIntent(t)
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
	in.Claims = append(in.Claims, DeclaredClaim{
		ID: "impossible", Statement: "proven by something that runs later", ProvenByGate: post,
	})
	_, err := BuildPlan(in, b, "x.json", Coverage())
	if err == nil || !strings.Contains(err.Error(), "after publication") {
		t.Fatalf("err = %v, want a refusal explaining that the lock is signed first", err)
	}
}

// TestSecondReleaseFixtureProvesThePipelineIsNotFirstReleaseOnly.
//
// The fixture differs from v0.3.0 in every dimension a real second release
// would: a version-kind predecessor, two predecessors, a smaller claim set and
// two platforms. If any of those broke the plan, this is where it shows.
func TestSecondReleaseFixtureProvesThePipelineIsNotFirstReleaseOnly(t *testing.T) {
	path := filepath.Join("testdata", "second-release", "v0.3.1.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	in, err := ParseIntent(b)
	if err != nil {
		t.Fatalf("the second-release fixture does not validate: %v", err)
	}
	if err := ValidateFilename(path, in); err != nil {
		t.Fatal(err)
	}

	second, err := BuildPlan(in, b, path, Coverage())
	if err != nil {
		t.Fatalf("the pipeline cannot plan a second release: %v", err)
	}

	fb, first := readCommittedIntent(t)
	firstPlan, err := BuildPlan(first, fb, "first.json", Coverage())
	if err != nil {
		t.Fatal(err)
	}

	if gatesOf(second) == gatesOf(firstPlan) {
		t.Errorf("both releases schedule %v; the plan is not derived from the intent",
			gatesOf(second))
	}
	if len(in.SupportedPredecessors) < 2 {
		t.Error("the fixture must declare more than one predecessor, or it does not exercise " +
			"the support window's shape")
	}
	if len(in.Platforms) < 2 {
		t.Error("the fixture must be multi-platform, or the index path in the descriptor " +
			"validation is never exercised")
	}
	sawVersionKind := false
	for _, p := range in.SupportedPredecessors {
		if p.Kind == "version" {
			sawVersionKind = true
		}
	}
	if !sawVersionKind {
		t.Error("the fixture must declare a version-kind predecessor: only the FIRST release's " +
			"predecessor is commit-anchored, and a pipeline that only handles commits handles " +
			"only the first release")
	}
}

func gatesOf(p ReleasePlan) string {
	out := make([]string, 0, len(p.PrePublication))
	for _, g := range p.PrePublication {
		out = append(out, g.Gate)
	}
	return strings.Join(out, ",")
}

// TestArtifactPlatformsRefusals. Each row is a way a release could ship for a
// platform set nobody declared — and donors are volunteer hardware, so "it
// worked on the runner" is not a property anybody can use.
func TestArtifactPlatformsRefusals(t *testing.T) {
	manifest := func(p *ocispec.Platform) ocispec.Descriptor {
		return ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.Digest("sha256:" + strings.Repeat("1", 64)),
			Size:      100, Platform: p,
		}
	}
	linuxAMD64 := &ocispec.Platform{OS: "linux", Architecture: "amd64"}
	linuxARM64 := &ocispec.Platform{OS: "linux", Architecture: "arm64"}
	index := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    digest.Digest("sha256:" + strings.Repeat("2", 64)),
		Size:      200,
	}

	cases := []struct {
		name     string
		declared []string
		artifact LockedArtifact
		want     string
	}{
		{
			name: "an index with no children", declared: []string{"linux/amd64"},
			artifact: LockedArtifact{Repository: "r", Descriptor: index},
			want:     "lists no child descriptors",
		},
		{
			name: "an index child with no platform", declared: []string{"linux/amd64"},
			artifact: LockedArtifact{Repository: "r", Descriptor: index,
				Manifests: []ocispec.Descriptor{manifest(nil)}},
			want: "carries no platform",
		},
		{
			name: "an index of nothing but attestations", declared: []string{"linux/amd64"},
			artifact: LockedArtifact{Repository: "r", Descriptor: index,
				Manifests: []ocispec.Descriptor{
					manifest(&ocispec.Platform{OS: "unknown", Architecture: "unknown"})}},
			want: "only children are attestations",
		},
		{
			name: "a single manifest with no platform", declared: []string{"linux/amd64"},
			artifact: LockedArtifact{Repository: "r", Descriptor: manifest(nil)},
			want:     "single manifest with no platform",
		},
		{
			name: "a single manifest that also lists children", declared: []string{"linux/amd64"},
			artifact: LockedArtifact{Repository: "r", Descriptor: manifest(linuxAMD64),
				Manifests: []ocispec.Descriptor{manifest(linuxARM64)}},
			want: "only an index has children",
		},
		{
			name: "a platform the intent never declared", declared: []string{"linux/amd64"},
			artifact: LockedArtifact{Repository: "r", Descriptor: index,
				Manifests: []ocispec.Descriptor{manifest(linuxAMD64), manifest(linuxARM64)}},
			want: "must be exactly equal",
		},
		{
			name: "a declared platform that was not built", declared: []string{"linux/amd64", "linux/arm64"},
			artifact: LockedArtifact{Repository: "r", Descriptor: index,
				Manifests: []ocispec.Descriptor{manifest(linuxAMD64)}},
			want: "must be exactly equal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateArtifactPlatforms(tc.declared, "nova-coordinator", tc.artifact)
			if err == nil {
				t.Fatalf("no refusal; %s must not be accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	// And the shapes that ARE correct.
	ok := LockedArtifact{Repository: "r", Descriptor: index,
		Manifests: []ocispec.Descriptor{
			manifest(linuxAMD64), manifest(linuxARM64),
			manifest(&ocispec.Platform{OS: "unknown", Architecture: "unknown"}),
		}}
	if err := ValidateArtifactPlatforms([]string{"linux/arm64", "linux/amd64"}, "x", ok); err != nil {
		t.Errorf("a correct multi-platform index was refused: %v", err)
	}
	single := LockedArtifact{Repository: "r", Descriptor: manifest(linuxAMD64)}
	if err := ValidateArtifactPlatforms([]string{"linux/amd64"}, "x", single); err != nil {
		t.Errorf("a correct single-platform manifest was refused: %v", err)
	}
}

// TestEvidenceRequiresAnIntentDigest. Two candidates can both be called v0.3.0
// with a different reviewed decision behind them, and a gate that passed
// against the first says nothing about the second.
func TestEvidenceRequiresAnIntentDigest(t *testing.T) {
	st := EvidenceStatement{
		Schema: EvidenceSchema, Gate: "upgrade-wire-e2e", RunnerClass: RunnerDocker,
		Outcome: "passed", Release: "v0.3.0", SourceCommit: "143c459",
		ArtifactDigests: map[string]string{"nova-node": "sha256:" + strings.Repeat("1", 64)},
		TestRevision:    "143c459",
		StartedAt:       time.Now().UTC(), FinishedAt: time.Now().UTC(),
	}
	if err := st.Validate(); err == nil || !strings.Contains(err.Error(), "intent_digest") {
		t.Fatalf("err = %v, want a refusal naming intent_digest", err)
	}
	st.IntentDigest = "sha256:" + strings.Repeat("9", 64)
	if err := st.Validate(); err != nil {
		t.Fatalf("a complete statement was refused: %v", err)
	}
}

// TestEvidenceMustRecordAConditionalClaimsCapabilities.
func TestEvidenceMustRecordAConditionalClaimsCapabilities(t *testing.T) {
	c := DeclaredClaim{
		ID: "baseline-donor-interop", ProvenByGate: "upgrade-wire-e2e",
		Statement:            "x",
		RequiresCapabilities: []string{"blob-transfer/v1"},
	}
	st := EvidenceStatement{
		Schema: EvidenceSchema, Gate: c.ProvenByGate, RunnerClass: RunnerDocker,
		Outcome: "passed", Release: "v0.3.0", SourceCommit: "143c459",
		IntentDigest:    "sha256:" + strings.Repeat("9", 64),
		ArtifactDigests: map[string]string{"nova-node": "sha256:" + strings.Repeat("1", 64)},
		TestRevision:    "143c459",
	}
	err := AssertEvidenceSupportsClaim(c, st, Coverage())
	if err == nil || !strings.Contains(err.Error(), "conditional on capability") {
		t.Fatalf("err = %v, want a refusal about the unexercised capability", err)
	}
	st.Capabilities = []string{"blob-transfer/v1"}
	if err := AssertEvidenceSupportsClaim(c, st, Coverage()); err != nil {
		t.Fatalf("evidence recording the capability was still refused: %v", err)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
