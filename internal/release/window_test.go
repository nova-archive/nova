package release

import (
	"strings"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/release/catalog"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// windowCatalog builds a target whose predecessor list is, newest first:
// N-1, N-2, N-3. Only the first two are on the predecessor branch.
func windowCatalog() Catalog {
	return catalog.Catalog{
		Version: "v0.6.0",
		SupportedPredecessors: []Predecessor{
			{Kind: "version", Version: "v0.5.0", Schema: 21, SupportEpoch: epoch.AddDate(0, 10, 0)},
			{Kind: "version", Version: "v0.4.0", Schema: 20, SupportEpoch: epoch.AddDate(0, 5, 0)},
			{Kind: "commit", Commit: "143c459", Schema: 18, SupportEpoch: epoch},
		},
	}
}

// TestSupportedWhileWithinTwoPredecessors: N-1 and N-2 are supported by the
// predecessor branch regardless of age.
func TestSupportedWhileWithinTwoPredecessors(t *testing.T) {
	c := windowCatalog()
	// Far past every age deadline.
	now := epoch.AddDate(5, 0, 0)

	for _, id := range []string{"v0.5.0", "v0.4.0"} {
		s := SupportedBy(c, id, now)
		if !s.Supported || !s.ByPredecessor {
			t.Errorf("%s: %+v; an immediate predecessor stays supported however old it is", id, s)
		}
		if s.ByAge {
			t.Errorf("%s: the age branch should have lapsed by %s", id, now.Format(time.DateOnly))
		}
	}
}

// TestSupportedWhileWithinSixMonths: the age branch alone is enough, even at
// N-3, which the predecessor branch does not reach.
func TestSupportedWhileWithinSixMonths(t *testing.T) {
	c := windowCatalog()
	now := epoch.AddDate(0, 3, 0) // three months after the baseline's epoch

	s := SupportedBy(c, "commit:143c459", now)
	if !s.Supported {
		t.Fatalf("baseline: %+v; three months in, the age branch holds", s)
	}
	if s.ByPredecessor {
		t.Error("the baseline is N-3 here and must not be on the predecessor branch")
	}
	if !s.ByAge {
		t.Error("the age branch must be what is holding")
	}
}

// TestExpiresOnlyWhenBothConditionsFail.
func TestExpiresOnlyWhenBothConditionsFail(t *testing.T) {
	c := windowCatalog()

	// N-3 and past its age deadline: both branches false.
	late := epoch.AddDate(1, 0, 0)
	s := SupportedBy(c, "commit:143c459", late)
	if s.Supported || s.ByPredecessor || s.ByAge {
		t.Fatalf("baseline at %s: %+v, want unsupported on both branches", late.Format(time.DateOnly), s)
	}
	if !strings.Contains(s.Explain(), "untested, not dead") {
		t.Errorf("Explain() = %q; unsupported must not read as evictable — existing replicas "+
			"are kept and nothing is evicted", s.Explain())
	}

	// One day before the deadline the age branch still holds.
	justBefore := AgeBranchDeadlineFor(c.SupportedPredecessors[2]).Add(-24 * time.Hour)
	if s := SupportedBy(c, "commit:143c459", justBefore); !s.Supported {
		t.Error("the age branch must hold right up to its deadline")
	}
}

// TestSixMonthsIsTheAgeBranchDeadlineNotTheExpiry. For a fixed target, an N-1
// artifact has no effective expiry; describing its six-month date as "when
// support ends" would be wrong for exactly the artifacts most likely still
// deployed.
func TestSixMonthsIsTheAgeBranchDeadlineNotTheExpiry(t *testing.T) {
	c := windowCatalog()
	nMinus1 := c.SupportedPredecessors[0]
	past := AgeBranchDeadlineFor(nMinus1).AddDate(1, 0, 0)

	s := SupportedBy(c, nMinus1.ID(), past)
	if !s.Supported {
		t.Fatal("an N-1 artifact past its age deadline is still supported")
	}
	if !strings.Contains(s.Explain(), "does not end support") {
		t.Errorf("Explain() = %q; the wording must not present the age deadline as an expiry", s.Explain())
	}
}

// TestBaselineExpiryUsesDeclaredEpoch. The precontract baseline has no release
// date — the remote tag namespace holds only two lightweight pre-contract refs
// — so its deadline is a stated date rather than an inference.
func TestBaselineExpiryUsesDeclaredEpoch(t *testing.T) {
	c := windowCatalog()
	baseline := c.SupportedPredecessors[2]
	if baseline.Kind != "commit" {
		t.Fatal("this test is about the commit-identified baseline")
	}
	want := baseline.SupportEpoch.Add(AgeBranchWindow)
	if got := AgeBranchDeadlineFor(baseline); !got.Equal(want) {
		t.Errorf("deadline = %s, want %s (epoch + six months, from the DECLARED epoch)", got, want)
	}
	s := SupportedBy(c, "commit:143c459", baseline.SupportEpoch.AddDate(0, 1, 0))
	if !s.Supported {
		t.Error("a commit-identified artifact must be evaluable at all; scenarios and the census " +
			"both key on this identity")
	}
}

// TestUnknownIsNotUnsupported. The interop contract is protocol plus
// capabilities; a version string was never it.
func TestUnknownIsNotUnsupported(t *testing.T) {
	s := SupportedBy(windowCatalog(), "v9.9.9", epoch)
	if s.Known {
		t.Fatal("v9.9.9 is not in the catalog")
	}
	if s.Supported {
		t.Error("an unknown artifact makes no support claim")
	}
	if !strings.Contains(s.Explain(), "protocol and capabilities") {
		t.Errorf("Explain() = %q; unknown must point at the real interop contract rather than "+
			"reading as incompatible", s.Explain())
	}
}

// TestWindowEvaluatesWithoutNetwork: the window reads the compiled-in catalog
// and a clock, and nothing else. T1.22 leaves it no other option.
func TestWindowEvaluatesWithoutNetwork(t *testing.T) {
	c := Compiled()
	if len(c.SupportedPredecessors) == 0 {
		t.Fatal("the shipped catalog declares no predecessors")
	}
	id := c.SupportedPredecessors[0].ID()
	s := SupportedBy(c, id, c.SupportEpoch)
	if !s.Supported || !s.ByPredecessor {
		t.Errorf("%s against the shipped catalog: %+v", id, s)
	}
}
