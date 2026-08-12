package release

import (
	"strings"
	"testing"
	"time"
)

func censusRow() NodeCensusRow {
	return NodeCensusRow{
		NodeID: "n1", DisplayName: "alice", Status: "active", TrustState: "trusted",
		SelectedProtocol: "fed/v1", LastSeenAt: time.Now(),
		SchemaAware: true,
	}
}

func stamped(observed time.Time) *time.Time { return &observed }

// TestCensusAxesAreIndependent. A donor can be current, stale and
// digest-mismatched at once; one enum forces a precedence order onto facts that
// have none, and whichever fact loses is the one the operator needed.
func TestCensusAxesAreIndependent(t *testing.T) {
	cat := windowCatalog()
	now := time.Now()

	r := censusRow()
	r.ReportedVersion = "v0.5.0" // an immediate predecessor: supported
	r.LastSeenAt = now.Add(-72 * time.Hour)
	r.ExpectedImageDigest = "sha256:" + strings.Repeat("a", 64)
	r.ReportedImageDigest = "sha256:" + strings.Repeat("b", 64)
	r.ContractObservedAt = stamped(now)
	r.EffectiveCapabilities = []string{"pin-change-log/v1", "snapshot/v1"}

	a := Classify(cat, r, now, time.Hour)

	if a.Version != VersionSupported {
		t.Errorf("version = %q, want supported", a.Version)
	}
	if a.Freshness != Stale {
		t.Errorf("freshness = %q, want stale", a.Freshness)
	}
	if a.ImageDigest != DigestMismatch {
		t.Errorf("image digest = %q, want mismatch", a.ImageDigest)
	}
	if a.ContractFresh != ContractRuntime {
		t.Errorf("contract freshness = %q, want runtime", a.ContractFresh)
	}
	if a.Protocol != Compatible {
		t.Errorf("protocol = %q, want compatible", a.Protocol)
	}
	// All four are simultaneously true, which is the point.
}

// TestBaselineDonorIsUnknownNotUnsupported. The baseline reports nothing, and
// there is no v0.2.0 product release to compare it to.
func TestBaselineDonorIsUnknownNotUnsupported(t *testing.T) {
	r := censusRow() // no reported version at all
	a := Classify(windowCatalog(), r, time.Now(), time.Hour)
	if a.Version != VersionUnknown {
		t.Errorf("version = %q, want unknown; a donor that told us nothing has made no claim", a.Version)
	}
	if a.Version == VersionUnsupported || a.Version == VersionInvalid {
		t.Error("unknown must not read as incompatible")
	}
}

// TestInvalidIsDistinguishedFromUnknown: nonsense from a donor and a donor
// older than this release's memory are different problems.
func TestInvalidIsDistinguishedFromUnknown(t *testing.T) {
	cat := windowCatalog()
	now := time.Now()

	r := censusRow()
	r.ReportedVersion = "not-a-version-at-all"
	if got := Classify(cat, r, now, time.Hour).Version; got != VersionInvalid {
		t.Errorf("version = %q, want invalid", got)
	}

	r.ReportedVersion = "v0.1.0" // well-formed, just not in the catalog
	if got := Classify(cat, r, now, time.Hour).Version; got != VersionUnknown {
		t.Errorf("version = %q, want unknown", got)
	}

	r.ReportedVersion = "commit:143c459" // the baseline's identity form
	if got := Classify(cat, r, now, time.Hour).Version; got == VersionInvalid {
		t.Error("a commit identity must not classify as invalid; it is how the baseline is named")
	}
}

// TestRuntimeContractFreshnessSeparatesLegacyFromRollback. The two produce the
// identical wire message; only the persisted marker tells them apart.
func TestRuntimeContractFreshnessSeparatesLegacyFromRollback(t *testing.T) {
	cat := windowCatalog()
	now := time.Now()

	legacy := censusRow() // marker nil, no reported version
	if got := Classify(cat, legacy, now, time.Hour).ContractFresh; got != ContractRegistrationOnly {
		t.Errorf("legacy donor = %q, want registration-only", got)
	}

	rolledBack := censusRow()
	rolledBack.ContractObservedAt = stamped(now.Add(-time.Hour)) // observed once
	rolledBack.ReportedVersion = ""                              // then stopped
	if got := Classify(cat, rolledBack, now, time.Hour).ContractFresh; got != ContractUnknown {
		t.Errorf("rolled-back donor = %q, want unknown", got)
	}

	current := censusRow()
	current.ContractObservedAt = stamped(now)
	current.ReportedVersion = "v0.5.0"
	if got := Classify(cat, current, now, time.Hour).ContractFresh; got != ContractRuntime {
		t.Errorf("current donor = %q, want runtime", got)
	}
}

// TestExpectedReportedMismatchIsAWarningOnly, on image AND bundle-lock
// separately: a donor can match one and not the other.
func TestExpectedReportedMismatchIsAWarningOnly(t *testing.T) {
	cat := windowCatalog()
	now := time.Now()
	same := "sha256:" + strings.Repeat("c", 64)

	r := censusRow()
	r.ContractObservedAt = stamped(now)
	r.ReportedVersion = "v0.5.0"
	r.ExpectedImageDigest, r.ReportedImageDigest = same, same
	r.ExpectedBundleLockDigest = "sha256:" + strings.Repeat("d", 64)
	r.ReportedBundleLockDigest = "sha256:" + strings.Repeat("e", 64)

	a := Classify(cat, r, now, time.Hour)
	if a.ImageDigest != DigestMatch {
		t.Errorf("image = %q, want match", a.ImageDigest)
	}
	if a.BundleLockDigest != DigestMismatch {
		t.Errorf("bundle lock = %q, want mismatch; the two are separate relations", a.BundleLockDigest)
	}
	// The mismatch does not touch any other axis.
	if a.Version != VersionSupported || a.Profile == Nonconforming {
		t.Errorf("a digest mismatch altered another axis: %+v", a)
	}

	// Nothing to compare against is `unavailable`, not `mismatch`.
	r.ExpectedImageDigest = ""
	if got := Classify(cat, r, now, time.Hour).ImageDigest; got != DigestUnavailable {
		t.Errorf("no expectation = %q, want unavailable", got)
	}
}

// TestOldestDonorIsByVersionNotLastSeen. min(last_seen_at) answers "who has
// been quiet longest", which is a liveness question in a version question's
// clothing.
func TestOldestDonorIsByVersionNotLastSeen(t *testing.T) {
	rows := []Axes{
		{VersionReported: "v0.10.0"},
		{VersionReported: "v0.9.0"},
		{VersionReported: "v0.2.0"},
		{VersionReported: "commit:143c459"}, // not comparable; skipped
		{VersionReported: ""},
	}
	if got := OldestByVersion(rows); got != "v0.2.0" {
		t.Errorf("oldest = %q, want v0.2.0 (SemVer precedence, not string order)", got)
	}
}

// TestCensusAgainstSchema18UsesBaselineColumnsOnly. `upgrade check` runs from
// the target admin BEFORE migration; a row from the baseline path must report
// every 0019-derived axis as `unavailable` rather than inventing one.
func TestCensusAgainstSchema18UsesBaselineColumnsOnly(t *testing.T) {
	r := censusRow()
	r.SchemaAware = false
	r.LegacyClientVersion = "v0.5.0"

	a := Classify(windowCatalog(), r, time.Now(), time.Hour)

	for name, got := range map[string]string{
		"image digest":       a.ImageDigest,
		"bundle lock digest": a.BundleLockDigest,
		"contract freshness": a.ContractFresh,
		"profile compliance": a.Profile,
	} {
		if got != Unavailable {
			t.Errorf("%s = %q at schema 18, want %q", name, got, Unavailable)
		}
	}
	// The axes schema 18 CAN answer are still answered.
	if a.Version != VersionSupported {
		t.Errorf("version = %q; client_version exists at schema 18 and must still classify", a.Version)
	}
	if a.Protocol != Compatible {
		t.Errorf("protocol = %q; selected_protocol exists at schema 18", a.Protocol)
	}
	if a.Freshness == "" {
		t.Error("freshness is a schema-18 column and must be classified")
	}
}

// TestUnavailableIsDistinctFromUnknown. "The schema cannot answer this" and
// "the donor told us nothing" are different facts, and collapsing them would
// make a preflight against schema 18 look like a fleet of silent donors.
func TestUnavailableIsDistinctFromUnknown(t *testing.T) {
	if Unavailable == VersionUnknown || Unavailable == ContractUnknown {
		t.Fatal("unavailable and unknown must be distinguishable")
	}
}

// TestPerRoleEligibilityComesFromEffectiveCapabilities.
func TestPerRoleEligibilityComesFromEffectiveCapabilities(t *testing.T) {
	cat := windowCatalog()
	cat.Profiles = CapabilityProfiles{
		Core: []string{"pin-change-log/v1", "snapshot/v1"},
		Roles: map[string][]string{
			"read-source":            {"read-source/v1", "blob-transfer/v1"},
			"assignment-destination": {"blob-transfer/v1"},
		},
	}
	now := time.Now()

	r := censusRow()
	r.ContractObservedAt = stamped(now)
	r.ReportedVersion = "v0.5.0"
	r.EffectiveCapabilities = []string{"pin-change-log/v1", "snapshot/v1", "blob-transfer/v1"}

	a := Classify(cat, r, now, time.Hour)
	if a.Profile != Compliant {
		t.Errorf("profile = %q; the core set is satisfied", a.Profile)
	}
	if a.Roles["read-source"] {
		t.Error("read-source needs read-source/v1, which this donor does not have")
	}
	if !a.Roles["assignment-destination"] {
		t.Error("assignment-destination needs only blob-transfer/v1")
	}
	// Missing an optional-role capability is not nonconformance.
	if a.Profile == Nonconforming {
		t.Error("missing an OPTIONAL-role capability must not read as profile nonconformance")
	}
}
