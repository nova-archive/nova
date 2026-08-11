package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/deploy"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// intentsDir is the checked-in intent directory, relative to this package.
func intentsDir() string { return filepath.Join("..", "..", "releases", "intent") }

func validIntent() Intent {
	return Intent{
		Schema:       IntentSchema,
		Version:      "v0.3.0",
		SupportEpoch: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		SupportUntil: time.Date(2027, 2, 11, 0, 0, 0, 0, time.UTC),
		SupportedPredecessors: []Predecessor{{
			Kind: "commit", Commit: "143c459", Schema: 18,
			SupportEpoch: time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC),
		}},
		TargetSchema: 19,
		Platforms:    []string{"linux/amd64"},
		Repositories: map[string]string{
			"nova-coordinator": "ghcr.io/nova-archive/nova-coordinator",
			"nova-node":        "ghcr.io/nova-archive/nova-node",
			"nova-admin":       "ghcr.io/nova-archive/nova-admin",
		},
		Sidecars: map[string]string{
			"nebula": deploy.DefaultNebulaImage,
			"kubo":   deploy.DefaultKuboImage,
		},
		FedProtocols:    []string{"fed/v1"},
		EnvelopeFormats: []int{1},
		Profiles:        CapabilityProfiles{Core: []string{"pin-change-log/v1", "snapshot/v1"}},
		Claims: []DeclaredClaim{{
			ID: "c1", Statement: "s", ProvenByGate: "upgrade-wire-e2e",
		}},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIntentRejectsNovaDigests. Intent is reviewed BEFORE any build exists, and
// stamping the version and OCI labels changes the image config digest and
// therefore the manifest digest — so a digest committed here could never be
// reproduced by the build it describes. This is the circular contract the
// intent/lock split exists to break.
func TestIntentRejectsNovaDigests(t *testing.T) {
	in := validIntent()
	in.Repositories["nova-node"] = "ghcr.io/nova-archive/nova-node@sha256:" + strings.Repeat("a", 64)
	err := in.Validate()
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("err = %v, want a refusal naming the digest", err)
	}
}

func TestIntentRejectsNovaTags(t *testing.T) {
	in := validIntent()
	in.Repositories["nova-node"] = "ghcr.io/nova-archive/nova-node:v0.3.0"
	if err := in.Validate(); err == nil {
		t.Fatal("intent must not choose tags; the release workflow does")
	}
}

// TestIntentRequiresExactSidecarRefs: third-party images are ALREADY published,
// so there is no reason to accept a mutable reference for one.
func TestIntentRequiresExactSidecarRefs(t *testing.T) {
	in := validIntent()
	in.Sidecars["kubo"] = "ipfs/kubo:v0.38.1"
	if err := in.Validate(); err == nil {
		t.Fatal("a sidecar without @sha256: must be rejected")
	}
}

// TestIntentClaimTypeHasNoEvidenceField is a COMPILE-LEVEL separation check:
// DeclaredClaim must not gain an evidence field, or a claim could acquire proof
// by assignment rather than by a gate running.
func TestIntentClaimTypeHasNoEvidenceField(t *testing.T) {
	b, err := json.Marshal(DeclaredClaim{ID: "x", Statement: "y", ProvenByGate: "g"})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"evidence", "statement_digest", "outcome", "runner_class"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("DeclaredClaim serializes %q; evidence exists only on the lock", forbidden)
		}
	}
}

func TestIntentRejectsBuildMetadata(t *testing.T) {
	in := validIntent()
	in.Version = "v0.3.0+143c459"
	err := in.Validate()
	if err == nil || !strings.Contains(err.Error(), "build metadata") {
		t.Fatalf("err = %v; `+` is not a legal Docker tag character and SemVer ignores "+
			"build metadata for precedence", err)
	}
}

func TestIntentFilenameMustMatchVersion(t *testing.T) {
	in := validIntent()
	if err := ValidateFilename("releases/intent/v0.2.9.json", in); err == nil {
		t.Fatal("a file whose name disagrees with its version must be rejected")
	}
	if err := ValidateFilename("releases/intent/v0.3.0.json", in); err != nil {
		t.Fatal(err)
	}
}

// TestIntentRejectsSchemaRegression: the schema is forward-only, so a
// predecessor cannot be ahead of the target.
func TestIntentRejectsSchemaRegression(t *testing.T) {
	in := validIntent()
	in.TargetSchema = 17
	err := in.Validate()
	if err == nil || !strings.Contains(err.Error(), "forward-only") {
		t.Fatalf("err = %v, want a forward-only refusal", err)
	}
}

func TestIntentRejectsUnknownAndTrailingJSON(t *testing.T) {
	good := mustJSON(t, validIntent())

	var m map[string]any
	if err := json.Unmarshal(good, &m); err != nil {
		t.Fatal(err)
	}
	m["targt_schema"] = 19 // a plausible typo
	if _, err := ParseIntent(mustJSON(t, m)); err == nil {
		t.Error("an unknown field must be rejected; silently ignoring a misspelling is how a " +
			"reviewed decision stops matching what is executed")
	}

	if _, err := ParseIntent(append(good, []byte("\n{}\n")...)); err == nil {
		t.Error("trailing content after the document must be rejected")
	}
}

func TestIntentDeclaresSupportWindowMetadata(t *testing.T) {
	in := validIntent()
	in.SupportEpoch = time.Time{}
	if err := in.Validate(); err == nil {
		t.Error("support_epoch drives the whole window and cannot be absent")
	}

	in = validIntent()
	in.SupportUntil = in.SupportEpoch.Add(-time.Hour)
	if err := in.Validate(); err == nil {
		t.Error("support_until before support_epoch must be rejected")
	}
}

// TestBaselineSupportEpochIsExplicit. Commit 143c459 has no release date — the
// remote tag namespace holds only two lightweight pre-contract refs — so its
// epoch must be declared, not inferred from a tag that does not exist.
func TestBaselineSupportEpochIsExplicit(t *testing.T) {
	in := validIntent()
	in.SupportedPredecessors[0].SupportEpoch = time.Time{}
	err := in.Validate()
	if err == nil || !strings.Contains(err.Error(), "support_epoch") {
		t.Fatalf("err = %v, want a refusal naming support_epoch", err)
	}
}

func TestPredecessorAcceptsCommitAndVersionButNotBoth(t *testing.T) {
	base := Predecessor{Schema: 18, SupportEpoch: time.Now()}

	c := base
	c.Kind, c.Commit = "commit", "143c459"
	if err := c.Validate(); err != nil {
		t.Errorf("a commit predecessor must be legal: %v", err)
	}
	if got := c.ID(); got != "commit:143c459" {
		t.Errorf("ID() = %q", got)
	}

	v := base
	v.Kind, v.Version = "version", "v0.3.0"
	if err := v.Validate(); err != nil {
		t.Errorf("a version predecessor must be legal: %v", err)
	}

	both := base
	both.Kind, both.Commit, both.Version = "commit", "143c459", "v0.3.0"
	if err := both.Validate(); err == nil {
		t.Error("a predecessor carrying both identities is ambiguous and must be rejected")
	}
}

func TestIntentRejectsAClaimWithNoGate(t *testing.T) {
	in := validIntent()
	in.Claims[0].ProvenByGate = ""
	if err := in.Validate(); err == nil {
		t.Fatal("a claim nothing must prove is not a claim")
	}
}

func TestIntentRejectsDuplicateClaimIDs(t *testing.T) {
	in := validIntent()
	in.Claims = append(in.Claims, in.Claims[0])
	if err := in.Validate(); err == nil {
		t.Fatal("duplicate claim ids make the coverage table ambiguous")
	}
}

// TestIntentDigestIsOfTheCheckedInBytes. Re-marshalling depends on field order,
// on omitempty, and on the encoder — none of which the reviewer looked at.
func TestIntentDigestIsOfTheCheckedInBytes(t *testing.T) {
	a := []byte(`{"schema":1}`)
	b := []byte(`{"schema": 1}`) // same value, different bytes
	if IntentDigest(a) == IntentDigest(b) {
		t.Fatal("the digest must be over the exact bytes, not over a parsed value")
	}
}

// TestCommittedIntentsAreValid is the gate, not just the validator: every
// checked-in intent must parse, validate, and be correctly named.
func TestCommittedIntentsAreValid(t *testing.T) {
	entries, err := os.ReadDir(intentsDir())
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		found++
		path := filepath.Join(intentsDir(), e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		in, err := ParseIntent(b)
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		if err := ValidateFilename(path, in); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
	}
	if found == 0 {
		t.Fatal("no committed intents found; this gate would pass vacuously")
	}
}

// TestCommittedIntentSidecarsMatchTheDeployDefaults is the drift gate. The
// donor topology's images are declared in internal/deploy AND in the intent; if
// they disagree, the release describes a topology the generator does not build.
func TestCommittedIntentSidecarsMatchTheDeployDefaults(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(intentsDir(), "v0.3.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := ParseIntent(b)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"nebula": deploy.DefaultNebulaImage,
		"kubo":   deploy.DefaultKuboImage,
	} {
		if got := in.Sidecars[name]; got != want {
			t.Errorf("intent sidecar %s = %q, but internal/deploy builds %q", name, got, want)
		}
	}
}

// TestCommittedIntentAgreesWithTheWireAndEnvelopeConstants. fed_protocols and
// envelope_formats are the interop contract; a release that names one the code
// does not implement is a claim nothing can satisfy.
func TestCommittedIntentAgreesWithTheCode(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(intentsDir(), "v0.3.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := ParseIntent(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.FedProtocols) != 1 || in.FedProtocols[0] != wire.ProtocolV1 {
		t.Errorf("fed_protocols = %v, want [%s] (internal/federation/wire/messages.go)",
			in.FedProtocols, wire.ProtocolV1)
	}
	if len(in.EnvelopeFormats) != 1 || in.EnvelopeFormats[0] != int(envelope.VersionV1) {
		t.Errorf("envelope_formats = %v, want [%d] (internal/envelope)",
			in.EnvelopeFormats, int(envelope.VersionV1))
	}
	if in.TargetSchema != 19 {
		t.Errorf("target_schema = %d, want 19", in.TargetSchema)
	}
}
