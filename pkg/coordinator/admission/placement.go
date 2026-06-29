package admission

import (
	"math"
	"sort"

	"github.com/google/uuid"
)

// Candidate is an eligible placement destination. Callers pass only
// status-eligible nodes (active + assignment_sync_state='current' +
// non-suspended); SelectDestination applies trust caps, reputation floor,
// capacity feasibility, anti-affinity, and the bandwidth-decoupled weight.
// FreeBytes is a self-reported HINT (the donor's storage_max_bytes / accept-refuse
// stays authoritative, D-M5-7-CAP). Dimension values are trusted for anti-affinity
// only when OperatorVerified.
type Candidate struct {
	NodeID           uuid.UUID
	FailureDomain    string
	Principal        string
	Provider         string
	ASN              string
	Region           string
	OperatorVerified bool
	FreeBytes        int64
	TrustState       string // probationary | trusted | suspended
	Reputation       float64
	PlacementWeight  float64
}

// Holder is an existing holder of the CID, for anti-affinity domain comparison.
type Holder struct {
	FailureDomain    string
	Principal        string
	Provider         string
	ASN              string
	Region           string
	OperatorVerified bool
}

// trustWeight caps placement by trust state (D-M5-7/T1.30). Suspended (and any
// unknown state) is zero — excluded entirely.
func trustWeight(ts string) float64 {
	switch ts {
	case "trusted":
		return 1.0
	case "probationary":
		return 0.5
	default:
		return 0.0
	}
}

// domainKey collapses unverified or empty dimension values into the single
// "unknown" bucket (D-M5-3a), so a node cannot manufacture diversity by leaving
// fields blank or varying self-declared data — only operator-verified values are
// distinct.
func domainKey(v string, verified bool) string {
	if !verified || v == "" {
		return "unknown"
	}
	return v
}

// SelectDestination picks the best eligible non-holder for the content class, or
// (uuid.Nil, false) when none qualifies. Anti-affinity is a lexicographic
// preference (failure_domain → principal → provider → asn → region), never a veto:
// if no candidate introduces a distinct domain, the best-weighted one is still
// chosen rather than blocking placement.
func SelectDestination(class string, sizeBytes int64, holders []Holder, candidates []Candidate, repFloor float64) (uuid.UUID, bool) {
	heldFD := map[string]bool{}
	heldPrin := map[string]bool{}
	heldProv := map[string]bool{}
	heldASN := map[string]bool{}
	heldRegion := map[string]bool{}
	for _, h := range holders {
		heldFD[domainKey(h.FailureDomain, h.OperatorVerified)] = true
		heldPrin[domainKey(h.Principal, h.OperatorVerified)] = true
		heldProv[domainKey(h.Provider, h.OperatorVerified)] = true
		heldASN[domainKey(h.ASN, h.OperatorVerified)] = true
		heldRegion[domainKey(h.Region, h.OperatorVerified)] = true
	}

	type scored struct {
		id      uuid.UUID
		novelty [5]bool // fd, principal, provider, asn, region not already held
		weight  float64
	}
	var elig []scored
	for _, c := range candidates {
		if trustWeight(c.TrustState) <= 0 { // suspended / unknown
			continue
		}
		if c.Reputation < repFloor {
			continue
		}
		if c.FreeBytes < sizeBytes { // capacity hint
			continue
		}
		// Probationary nodes are never the sole or second copy of important data.
		if class == "important" && c.TrustState == "probationary" && len(holders) < 2 {
			continue
		}
		elig = append(elig, scored{
			id: c.NodeID,
			novelty: [5]bool{
				!heldFD[domainKey(c.FailureDomain, c.OperatorVerified)],
				!heldPrin[domainKey(c.Principal, c.OperatorVerified)],
				!heldProv[domainKey(c.Provider, c.OperatorVerified)],
				!heldASN[domainKey(c.ASN, c.OperatorVerified)],
				!heldRegion[domainKey(c.Region, c.OperatorVerified)],
			},
			// Bandwidth-decoupled: ~sqrt(free) × trust × placement_weight. No egress
			// term — bandwidth governs repair-source selection only.
			weight: math.Sqrt(float64(c.FreeBytes)) * trustWeight(c.TrustState) * c.PlacementWeight,
		})
	}
	if len(elig) == 0 {
		return uuid.Nil, false
	}
	sort.SliceStable(elig, func(i, j int) bool {
		a, b := elig[i], elig[j]
		for d := range 5 {
			if a.novelty[d] != b.novelty[d] {
				return a.novelty[d] // novel (true) sorts first
			}
		}
		if a.weight != b.weight {
			return a.weight > b.weight
		}
		return a.id.String() < b.id.String()
	})
	return elig[0].id, true
}
