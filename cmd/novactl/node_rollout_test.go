package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/nova-archive/nova/internal/release"
)

// rolloutFixture writes a matching intent + lock pair to a temp dir and returns
// their paths plus the lock's digest.
func rolloutFixture(t *testing.T) (intentPath, lockPath, lockDigest string) {
	t.Helper()
	dir := t.TempDir()

	src, err := os.ReadFile(filepath.Join("..", "..", "releases", "intent", "v0.3.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := release.ParseIntent(src)
	if err != nil {
		t.Fatal(err)
	}
	intentPath = filepath.Join(dir, "intent.json")
	if err := os.WriteFile(intentPath, src, 0o600); err != nil {
		t.Fatal(err)
	}

	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    digest.Digest("sha256:" + strings.Repeat("1", 64)),
		Size:      512,
	}
	arts := map[string]release.LockedArtifact{}
	for _, name := range release.ArtifactNames {
		arts[name] = release.LockedArtifact{Repository: in.Repositories[name], Descriptor: desc}
	}
	proven := make([]release.ProvenClaim, 0, len(in.Claims))
	for _, c := range in.Claims {
		proven = append(proven, release.ProvenClaim{
			ID: c.ID, Statement: c.Statement, Gate: c.ProvenByGate,
			Evidence: []release.EvidenceRef{{
				StatementDigest: "sha256:" + strings.Repeat("2", 64),
				Gate:            c.ProvenByGate, Outcome: "passed", RunnerClass: "rc-docker",
				At: time.Now().UTC(),
			}},
		})
	}
	payload := map[string]string{
		release.MemberIntent:     release.IntentDigest(src),
		release.MemberPolicy:     "sha256:" + strings.Repeat("3", 64),
		release.MemberComposeEnv: "sha256:" + strings.Repeat("4", 64),
		release.MemberUpgrading:  "sha256:" + strings.Repeat("5", 64),
		release.MemberBootstrap:  "sha256:" + strings.Repeat("6", 64),
	}
	lock := release.Lock{
		Schema: release.LockSchema, Version: in.Version,
		IntentDigest: release.IntentDigest(src),
		SourceCommit: "143c4590000", BuiltAt: time.Now().UTC(),
		TargetSchema: in.TargetSchema,
		Artifacts:    arts, Sidecars: in.Sidecars,
		ProvenClaims: proven, Payload: payload,
		DonorLockDigest: "sha256:" + strings.Repeat("7", 64),
	}
	b, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	lockPath = filepath.Join(dir, "lock.json")
	if err := os.WriteFile(lockPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return intentPath, lockPath, release.LockDigest(b)
}

// TestRolloutAuthorizeRequiresAVerifiedLock. `--lock <file>` means nothing
// across a process boundary: a caller who can choose the path can choose the
// contents. The digest is the identity.
func TestRolloutAuthorizeRequiresAVerifiedLock(t *testing.T) {
	intentPath, lockPath, digest := rolloutFixture(t)

	if _, err := loadVerifiedLock(lockPath, intentPath, digest); err != nil {
		t.Fatalf("the matching digest must be accepted: %v", err)
	}

	wrong := "sha256:" + strings.Repeat("f", 64)
	_, err := loadVerifiedLock(lockPath, intentPath, wrong)
	if err == nil || !strings.Contains(err.Error(), "did not check") {
		t.Fatalf("err = %v, want a refusal to authorize an unverified document", err)
	}
}

// TestRolloutAuthorizeDetectsATamperedLock.
func TestRolloutAuthorizeDetectsATamperedLock(t *testing.T) {
	intentPath, lockPath, digest := rolloutFixture(t)

	b, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, append(b, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadVerifiedLock(lockPath, intentPath, digest); err == nil {
		t.Fatal("a single trailing byte changes the document and must be refused")
	}
}

// TestRolloutAuthorizeChecksTheIntentAgainstTheLocksPayload. The intent is
// authenticated by the lock's payload map — the acyclic graph doing the work a
// second signature would otherwise have to.
func TestRolloutAuthorizeChecksTheIntentAgainstTheLocksPayload(t *testing.T) {
	intentPath, lockPath, digest := rolloutFixture(t)

	src, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(src, &m); err != nil {
		t.Fatal(err)
	}
	m["target_schema"] = 19 // same value, different bytes
	altered, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(intentPath, altered, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = loadVerifiedLock(lockPath, intentPath, digest)
	if err == nil {
		t.Fatal("an intent whose bytes differ from the lock's record must be refused, even " +
			"when it parses to the same value")
	}
}

// TestRolloutAuthorizeUsesBothDigestsFromTheLock. Image digest and bundle-lock
// digest are separate relations: a donor can match one and not the other, and
// half-authorizing leaves the census comparing against a mixture.
func TestRolloutAuthorizeUsesBothDigestsFromTheLock(t *testing.T) {
	intentPath, lockPath, digest := rolloutFixture(t)
	lock, err := loadVerifiedLock(lockPath, intentPath, digest)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Artifacts["nova-node"].Descriptor.Digest == "" {
		t.Error("no nova-node image digest to authorize")
	}
	if lock.DonorLockDigest == "" {
		t.Error("no donor bundle-lock digest to authorize")
	}
}

// TestRolloutAuthorizeParses covers the flag surface without a database.
func TestRolloutAuthorizeParses(t *testing.T) {
	checkFlagsOnly = true
	t.Cleanup(func() { checkFlagsOnly = false })

	intentPath, lockPath, digest := rolloutFixture(t)
	err := cmdNodeRollout([]string{"authorize",
		"--id", "19e8f7b9-2ddc-4d12-8260-14dea6d39edf",
		"--lock", lockPath, "--intent", intentPath, "--expect-lock-digest", digest})
	if err != nil && err.Error() != errCheckFlagsOK.Error() {
		t.Fatalf("err = %v, want the flags to parse and stop short of the database", err)
	}
}

// TestRolloutAuthorizeRequiresEveryInput runs WITHOUT checkFlagsOnly on
// purpose. parseFlags returns errCheckFlagsOK the moment that flag is set, so
// under it every one of these would "fail" without the validation ever
// running — a test that passes for the wrong reason. Each case below stops at
// its own required-input check, all of which precede any file or database
// access.
func TestRolloutAuthorizeRequiresEveryInput(t *testing.T) {
	for _, args := range [][]string{
		{"authorize", "--lock", "l", "--intent", "i", "--expect-lock-digest", "d"},                    // no id
		{"authorize", "--id", "19e8f7b9-2ddc-4d12-8260-14dea6d39edf", "--intent", "i"},                // no lock
		{"authorize", "--id", "19e8f7b9-2ddc-4d12-8260-14dea6d39edf", "--lock", "l"},                  // no intent
		{"authorize", "--id", "19e8f7b9-2ddc-4d12-8260-14dea6d39edf", "--lock", "l", "--intent", "i"}, // no digest
	} {
		if err := cmdNodeRollout(args); err == nil {
			t.Errorf("%v was accepted with an input missing", args)
		}
	}
}

// TestRolloutAuthorizeAbsentFromDonorBundle. The volunteer's update script
// holds no coordinator admin authority; this is operator-side only.
func TestRolloutAuthorizeAbsentFromDonorBundle(t *testing.T) {
	root := filepath.Join("..", "..", "internal", "deploy", "templates")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "rollout authorize") {
			t.Errorf("%s references `rollout authorize`; the two parties are separate and the "+
				"volunteer has no admin authority", e.Name())
		}
	}
}
