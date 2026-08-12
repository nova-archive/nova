package release

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func hashOf(s string) string { return "sha256:" + strings.Repeat(s, 64/len(s))[:64] }

func descriptor() ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    digest.Digest("sha256:" + strings.Repeat("1", 64)),
		Size:      1234,
	}
}

// childFor is one platform's manifest inside an index. The fixture is an index
// because that is what a real multi-platform build produces, and because an
// index is the shape whose platform set the lock has to derive rather than read
// off a single descriptor.
func childFor(platform string) ocispec.Descriptor {
	parts := strings.Split(platform, "/")
	p := &ocispec.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) > 2 {
		p.Variant = parts[2]
	}
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.Digest("sha256:" + strings.Repeat("2", 64)),
		Size:      567,
		Platform:  p,
	}
}

// artifactFor builds a locked artifact that publishes exactly the declared
// platforms, plus the attestation manifest a real BuildKit index carries.
func artifactFor(repo string, platforms []string) LockedArtifact {
	children := make([]ocispec.Descriptor, 0, len(platforms)+1)
	for _, p := range platforms {
		children = append(children, childFor(p))
	}
	children = append(children, ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.Digest("sha256:" + strings.Repeat("3", 64)),
		Size:      89,
		Platform:  &ocispec.Platform{OS: "unknown", Architecture: "unknown"},
	})
	return LockedArtifact{Repository: repo, Descriptor: descriptor(), Manifests: children}
}

func validLock(t *testing.T, in Intent, intentBytes []byte) Lock {
	t.Helper()
	arts := map[string]LockedArtifact{}
	for _, name := range ArtifactNames {
		arts[name] = artifactFor(in.Repositories[name], in.Platforms)
	}
	proven := make([]ProvenClaim, 0, len(in.Claims))
	for _, c := range in.Claims {
		proven = append(proven, ProvenClaim{
			ID: c.ID, Statement: c.Statement, Gate: c.ProvenByGate,
			Evidence: []EvidenceRef{{
				StatementDigest: hashOf("a"), Gate: c.ProvenByGate,
				Outcome: "passed", RunnerClass: "rc-docker", At: time.Now().UTC(),
			}},
		})
	}
	payload := map[string]string{}
	for _, m := range requiredMembers {
		payload[m] = hashOf("b")
	}
	payload[MemberHashes] = hashOf("c")
	payload["evidence/upgrade-wire-e2e.json"] = hashOf("d")

	return Lock{
		Schema: LockSchema, Version: in.Version,
		IntentDigest: IntentDigest(intentBytes),
		SourceCommit: "143c4590000", BuiltAt: time.Now().UTC(),
		TargetSchema: in.TargetSchema,
		Artifacts:    arts, Sidecars: in.Sidecars,
		ProvenClaims: proven, Payload: payload,
		DonorLockDigest: hashOf("e"),
	}
}

func lockFixture(t *testing.T) (Intent, []byte, Lock) {
	t.Helper()
	in := validIntent()
	b := mustJSON(t, in)
	return in, b, validLock(t, in, b)
}

// TestLockRequiresAllThreeArtifacts. nova-admin is the ONLY container that
// mounts nova-fedpki; releasing two of three leaves issuance authority running
// unsigned, unpinned bytes.
func TestLockRequiresAllThreeArtifacts(t *testing.T) {
	in, ib, l := lockFixture(t)
	delete(l.Artifacts, "nova-admin")
	err := l.Validate(in, ib)
	if err == nil || !strings.Contains(err.Error(), "nova-admin") {
		t.Fatalf("err = %v, want a refusal naming nova-admin", err)
	}
}

func TestLockRejectsWrongRepository(t *testing.T) {
	in, ib, l := lockFixture(t)
	a := l.Artifacts["nova-node"]
	a.Repository = "ghcr.io/someone-else/nova-node"
	l.Artifacts["nova-node"] = a
	if err := l.Validate(in, ib); err == nil {
		t.Fatal("a lock naming a repository the intent never declared must be rejected")
	}
}

// TestLockRequiresDescriptorsNotBareDigests. `docker buildx imagetools create`
// wraps a single-platform manifest in a NEW index unless told otherwise, which
// changes the digest; the mediaType is what lets the read-back tell the two
// apart.
func TestLockRequiresDescriptorsNotBareDigests(t *testing.T) {
	in, ib, l := lockFixture(t)

	bare := l.Artifacts["nova-node"]
	bare.Descriptor.MediaType = ""
	l.Artifacts["nova-node"] = bare
	err := l.Validate(in, ib)
	if err == nil || !strings.Contains(err.Error(), "mediaType") {
		t.Fatalf("err = %v, want a refusal naming mediaType", err)
	}

	_, ib2, l2 := lockFixture(t)
	sizeless := l2.Artifacts["nova-node"]
	sizeless.Descriptor.Size = 0
	l2.Artifacts["nova-node"] = sizeless
	if err := l2.Validate(in, ib2); err == nil {
		t.Fatal("a descriptor with no size cannot be verified against the registry")
	}
}

func TestLockMustReferenceItsIntentDigest(t *testing.T) {
	in, ib, l := lockFixture(t)
	l.IntentDigest = hashOf("f")
	err := l.Validate(in, ib)
	if err == nil || !strings.Contains(err.Error(), "intent_digest") {
		t.Fatalf("err = %v, want a refusal naming intent_digest", err)
	}
}

// TestLockBindsEveryClaimToSignedEvidence: content digests, not run IDs. Logs
// expire and a run ID proves nothing about what the run concluded.
func TestLockBindsEveryClaimToSignedEvidence(t *testing.T) {
	in, ib, l := lockFixture(t)
	l.ProvenClaims[0].Evidence = nil
	if err := l.Validate(in, ib); err == nil {
		t.Error("a claim with no evidence is a declared claim wearing the lock's type")
	}

	_, ib2, l2 := lockFixture(t)
	l2.ProvenClaims[0].Evidence[0].StatementDigest = "run-8675309"
	err := l2.Validate(in, ib2)
	if err == nil || !strings.Contains(err.Error(), "run id") {
		t.Errorf("err = %v, want a refusal explaining that evidence is bound by content digest", err)
	}

	_, ib3, l3 := lockFixture(t)
	l3.ProvenClaims[0].Evidence[0].Outcome = "skipped"
	if err := l3.Validate(in, ib3); err == nil {
		t.Error("a skipped gate cannot prove a claim; a claim is required by definition")
	}

	_, ib4, l4 := lockFixture(t)
	l4.ProvenClaims[0].Evidence[0].RunnerClass = ""
	if err := l4.Validate(in, ib4); err == nil {
		t.Error("without a runner class a static gate can silently satisfy an executed claim")
	}
}

func TestLockRejectsAnUndeclaredOrUnprovenClaim(t *testing.T) {
	in, ib, l := lockFixture(t)
	l.ProvenClaims = append(l.ProvenClaims, ProvenClaim{
		ID: "smuggled", Statement: "s", Gate: "g",
		Evidence: []EvidenceRef{{StatementDigest: hashOf("a"), Gate: "g", Outcome: "passed", RunnerClass: "rc-docker"}},
	})
	if err := l.Validate(in, ib); err == nil {
		t.Error("a claim nobody reviewed must not appear only on the lock")
	}

	_, ib2, l2 := lockFixture(t)
	l2.ProvenClaims = nil
	if err := l2.Validate(in, ib2); err == nil {
		t.Error("a declared claim with no proof must block the release")
	}
}

// TestLockHashesEveryBundleMember, including scripts/nova-release: the
// bootstrap script runs only after the authenticated lock names its exact hash.
func TestLockHashesEveryBundleMember(t *testing.T) {
	in, ib, l := lockFixture(t)
	for _, m := range requiredMembers {
		l2 := l
		l2.Payload = map[string]string{}
		for k, v := range l.Payload {
			l2.Payload[k] = v
		}
		delete(l2.Payload, m)
		err := l2.Validate(in, ib)
		if err == nil || !strings.Contains(err.Error(), m) {
			t.Errorf("removing %s: err = %v, want a refusal naming it", m, err)
		}
	}
}

// TestLockCannotHashItsOwnAuthenticationEnvelope is the acyclicity rule
// (D-M7.3-2d). The Sigstore bundle CONTAINS the signature over the lock, so a
// lock hashing it would have to be written after it was signed.
func TestLockCannotHashItsOwnAuthenticationEnvelope(t *testing.T) {
	in, ib, _ := lockFixture(t)
	for _, path := range excludedFromPayload {
		_, ib2, l := lockFixture(t)
		l.Payload[path] = hashOf("a")
		err := l.Validate(in, ib2)
		if err == nil || !strings.Contains(err.Error(), "cyclic") {
			t.Errorf("payload[%s]: err = %v, want a cyclic-graph refusal", path, err)
		}
		_ = ib
	}
}

func TestLockRejectsUnknownAndTrailingJSON(t *testing.T) {
	in, ib, l := lockFixture(t)
	good := mustJSON(t, l)

	var m map[string]any
	if err := json.Unmarshal(good, &m); err != nil {
		t.Fatal(err)
	}
	m["intnt_digest"] = "x"
	if _, err := ParseLock(mustJSON(t, m), in, ib); err == nil {
		t.Error("an unknown field must be rejected")
	}
	if _, err := ParseLock(append(good, []byte("\n{}")...), in, ib); err == nil {
		t.Error("trailing content must be rejected")
	}
	if _, err := ParseLock(good, in, ib); err != nil {
		t.Errorf("the valid fixture must parse: %v", err)
	}
}

// TestOnlyFinalizedLockAssertsProof: the two claim types are structurally
// distinct, so a declared claim cannot be assigned where a proven one belongs.
func TestOnlyFinalizedLockAssertsProof(t *testing.T) {
	var declared any = DeclaredClaim{}
	if _, ok := declared.(ProvenClaim); ok {
		t.Fatal("DeclaredClaim and ProvenClaim must not be interchangeable")
	}
	b, err := json.Marshal(ProvenClaim{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "evidence") {
		t.Fatal("ProvenClaim must carry evidence; that is what makes it proven")
	}
}

// TestDonorProjectionIsDeterministic. The donor lock is a projection of the
// release lock, not a separately signed document, so its digest must be a
// function of the lock alone.
func TestDonorProjectionIsDeterministic(t *testing.T) {
	_, _, l := lockFixture(t)
	p, err := DonorProjectionOf(l)
	if err != nil {
		t.Fatal(err)
	}
	first, err := DonorProjectionBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := DonorProjectionBytes(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatal("the donor projection is not byte-stable")
		}
	}
	if !strings.Contains(string(first), l.Artifacts["nova-node"].Ref()) {
		t.Error("the projection must pin the node image by digest")
	}
	for _, want := range []string{l.Sidecars["nebula"], l.Sidecars["kubo"]} {
		if !strings.Contains(string(first), want) {
			t.Errorf("the projection must carry %s; the donor topology is node + Nebula + Kubo "+
				"and an update covers all three", want)
		}
	}
}

func TestLockDigestIsOfTheExactBytes(t *testing.T) {
	if LockDigest([]byte(`{"a":1}`)) == LockDigest([]byte(`{"a": 1}`)) {
		t.Fatal("the lock digest must be over exact bytes: a path is not an identity, and " +
			"every consumer re-hashes what it was handed")
	}
}
