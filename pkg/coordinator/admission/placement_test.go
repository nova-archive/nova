package admission

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func cand(id string, fd string, verified bool, free int64, trust string) Candidate {
	return Candidate{
		NodeID:           uuid.MustParse(id),
		FailureDomain:    fd,
		OperatorVerified: verified,
		FreeBytes:        free,
		TrustState:       trust,
		Reputation:       1.0,
		PlacementWeight:  1.0,
	}
}

func TestAntiAffinityPrefersDistinctVerifiedDomain(t *testing.T) {
	// Holder is in verified domain "A". A candidate in verified "B" must win over a
	// candidate in "A", regardless of (equal) weight.
	holders := []Holder{{FailureDomain: "A", OperatorVerified: true}}
	candidates := []Candidate{
		cand("11111111-1111-1111-1111-111111111111", "A", true, 1_000_000, "trusted"),
		cand("22222222-2222-2222-2222-222222222222", "B", true, 1_000_000, "trusted"),
	}
	got, ok := SelectDestination("normal", 100, holders, candidates, 0.5)
	require.True(t, ok)
	require.Equal(t, "22222222-2222-2222-2222-222222222222", got.String(), "distinct domain preferred")
}

func TestUnverifiedDomainsCollapseToUnknownNotDiverse(t *testing.T) {
	// Holder collapses to the unknown bucket. An unverified candidate (also unknown)
	// is NOT diverse vs the holder; a verified-distinct candidate is. The verified
	// one must win — a node cannot manufacture diversity by leaving fields blank.
	holders := []Holder{{FailureDomain: "", OperatorVerified: false}}
	candidates := []Candidate{
		cand("11111111-1111-1111-1111-111111111111", "X", false, 9_000_000, "trusted"), // unverified ⇒ unknown
		cand("22222222-2222-2222-2222-222222222222", "B", true, 1_000_000, "trusted"),  // verified, novel
	}
	got, ok := SelectDestination("normal", 100, holders, candidates, 0.5)
	require.True(t, ok)
	require.Equal(t, "22222222-2222-2222-2222-222222222222", got.String(),
		"unverified domains collapse to unknown and are not treated as diverse")
}

func TestProbationaryNeverSoleOrSecondImportant(t *testing.T) {
	// important class, only one existing holder ⇒ a probationary candidate may not
	// become the second copy. With only a probationary candidate, no selection.
	holders := []Holder{{FailureDomain: "A", OperatorVerified: true}}
	candidates := []Candidate{
		cand("11111111-1111-1111-1111-111111111111", "B", true, 1_000_000, "probationary"),
	}
	_, ok := SelectDestination("important", 100, holders, candidates, 0.5)
	require.False(t, ok, "probationary cannot be the second copy of important")

	// A trusted candidate is fine.
	candidates = append(candidates, cand("22222222-2222-2222-2222-222222222222", "C", true, 1_000_000, "trusted"))
	got, ok := SelectDestination("important", 100, holders, candidates, 0.5)
	require.True(t, ok)
	require.Equal(t, "22222222-2222-2222-2222-222222222222", got.String())
}

func TestAntiAffinityNeverVetoesWhenNoDiverseCapacity(t *testing.T) {
	// All candidates share the holder's domain; anti-affinity is a preference, not a
	// veto, so one is still selected.
	holders := []Holder{{FailureDomain: "A", OperatorVerified: true}}
	candidates := []Candidate{
		cand("11111111-1111-1111-1111-111111111111", "A", true, 2_000_000, "trusted"),
		cand("22222222-2222-2222-2222-222222222222", "A", true, 1_000_000, "trusted"),
	}
	got, ok := SelectDestination("normal", 100, holders, candidates, 0.5)
	require.True(t, ok, "never veto into no placement")
	require.Equal(t, "11111111-1111-1111-1111-111111111111", got.String(), "ties break to higher free capacity")
}

func TestReputationFloorExcludes(t *testing.T) {
	holders := []Holder{}
	low := cand("11111111-1111-1111-1111-111111111111", "A", true, 1_000_000, "trusted")
	low.Reputation = 0.3
	got, ok := SelectDestination("normal", 100, holders, []Candidate{low}, 0.5)
	require.False(t, ok, "below reputation floor excluded")
	require.Equal(t, uuid.Nil, got)
}

func TestCapacityHintExcludesInfeasible(t *testing.T) {
	holders := []Holder{}
	small := cand("11111111-1111-1111-1111-111111111111", "A", true, 50, "trusted")
	_, ok := SelectDestination("normal", 100, holders, []Candidate{small}, 0.5)
	require.False(t, ok, "candidate without room for the blob is infeasible")
}

func TestWeightUsesTrustNotBandwidth(t *testing.T) {
	// Same free capacity: a trusted node outranks a probationary one (trust is a
	// multiplier). There is no bandwidth field — placement cannot weight on egress.
	holders := []Holder{}
	candidates := []Candidate{
		cand("11111111-1111-1111-1111-111111111111", "A", true, 1_000_000, "probationary"),
		cand("22222222-2222-2222-2222-222222222222", "B", true, 1_000_000, "trusted"),
	}
	got, ok := SelectDestination("normal", 100, holders, candidates, 0.5)
	require.True(t, ok)
	require.Equal(t, "22222222-2222-2222-2222-222222222222", got.String(), "trusted outranks probationary at equal free")
}
