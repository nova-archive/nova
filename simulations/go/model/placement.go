//go:build novasim

package model

import (
	"math"
	"math/rand"
)

// FileSizes returns a log-normal distribution of envelope sizes in bytes,
// clamped to [10 KiB, 50 MiB], matching simulations/orchestrator_resilience.py.
// medianBytes sets the median; the long tail models high-resolution scans and
// raw archival uploads.
func FileSizes(numCIDs int, medianBytes float64, seed int64) []float64 {
	rng := rand.New(rand.NewSource(seed))
	const sigma = 1.5
	minB, maxB := 10.0*KiB, 50.0*MiB
	mu := math.Log(medianBytes)
	out := make([]float64, numCIDs)
	for i := range out {
		// log-normal: exp(Normal(mu, sigma)).
		s := math.Exp(rng.NormFloat64()*sigma + mu)
		out[i] = math.Max(minB, math.Min(maxB, s))
	}
	return out
}

// PlacementStrategy assigns a selection weight to a candidate node given the
// holders already chosen for the same CID. A higher weight means more likely
// to be picked. Strategies never hard-refuse a node (weight may be small but
// the sampler can still pick it when nothing better remains) — this is the
// "alert, not prevent" stance applied to placement: anti-affinity is a
// preference, not a veto.
type PlacementStrategy interface {
	Name() string
	Weight(n *Node, chosen []*Node) float64
}

// BandwidthWeighted is the status-quo strategy from
// orchestrator_resilience.py: weight == daily egress budget, with no
// failure-domain awareness. It is efficient at steady state but manufactures
// the targeted-attack fragility the design doc analyses (capacity → centrality).
type BandwidthWeighted struct{}

func (BandwidthWeighted) Name() string                      { return "bandwidth-weighted" }
func (BandwidthWeighted) Weight(n *Node, _ []*Node) float64 { return n.Profile.BudgetBytesPerDay }

// DiversityOptimized decouples placement weight from bandwidth (ChatGPT's
// Priority 2): weight ~ sqrt(free capacity) × trust, multiplied by SOFT
// anti-affinity penalties when a candidate shares a failure domain with an
// already-chosen holder. Bandwidth is deliberately absent — it governs repair
// SOURCE selection (heal.go), not how much of the corpus a node accretes.
type DiversityOptimized struct {
	SameHostMult      float64
	SameProviderMult  float64
	SameASNMult       float64
	SameRegionMult    float64
	SamePrincipalMult float64
}

// DefaultDiversityOptimized returns sensible soft penalties. Same-host and
// same-principal are strongly discouraged (a shared host/operator is a single
// failure); same-region is mildly discouraged.
func DefaultDiversityOptimized() DiversityOptimized {
	return DiversityOptimized{
		SameHostMult:      0.001,
		SameProviderMult:  0.02,
		SameASNMult:       0.05,
		SameRegionMult:    0.5,
		SamePrincipalMult: 0.001,
	}
}

func (DiversityOptimized) Name() string { return "diversity-optimized" }

func (d DiversityOptimized) Weight(n *Node, chosen []*Node) float64 {
	w := math.Sqrt(n.FreeBytes()) * n.Trust
	if w <= 0 {
		// Even a full node keeps a tiny non-zero weight so the sampler can
		// still place when every candidate is constrained (never hard-fail).
		w = 1e-9
	}
	for _, c := range chosen {
		w *= d.affinity(n, c)
	}
	return w
}

func (d DiversityOptimized) affinity(n, c *Node) float64 {
	m := 1.0
	if n.Domain.Host == c.Domain.Host {
		m *= d.SameHostMult
	}
	if n.Domain.Principal == c.Domain.Principal {
		m *= d.SamePrincipalMult
	}
	if n.Domain.Provider == c.Domain.Provider {
		m *= d.SameProviderMult
	}
	if n.Domain.ASN == c.Domain.ASN {
		m *= d.SameASNMult
	}
	if n.Domain.Region == c.Domain.Region {
		m *= d.SameRegionMult
	}
	return m
}

// selectDistinct picks up to k distinct nodes from candidates using the
// strategy's weights, recomputing weights as the chosen set grows so
// anti-affinity takes effect. Weighted sampling without replacement.
func selectDistinct(rng *rand.Rand, candidates []*Node, k int, strat PlacementStrategy) []*Node {
	if k > len(candidates) {
		k = len(candidates)
	}
	chosen := make([]*Node, 0, k)
	pool := make([]*Node, len(candidates))
	copy(pool, candidates)

	for len(chosen) < k && len(pool) > 0 {
		var total float64
		weights := make([]float64, len(pool))
		for i, n := range pool {
			w := strat.Weight(n, chosen)
			if w < 0 {
				w = 0
			}
			weights[i] = w
			total += w
		}
		var idx int
		if total <= 0 {
			idx = rng.Intn(len(pool))
		} else {
			pick := rng.Float64() * total
			cum := 0.0
			idx = len(pool) - 1
			for i, w := range weights {
				cum += w
				if cum >= pick {
					idx = i
					break
				}
			}
		}
		chosen = append(chosen, pool[idx])
		pool[idx] = pool[len(pool)-1]
		pool = pool[:len(pool)-1]
	}
	return chosen
}

// AssignStorage places each CID on R distinct nodes per the strategy, updating
// each node's Pins and UsedBytes. Returns pins[cid] = node indices (== IDs).
func AssignStorage(nodes []*Node, sizes []float64, R int, strat PlacementStrategy, seed int64) [][]int {
	rng := rand.New(rand.NewSource(seed * 31337))
	pins := make([][]int, len(sizes))
	for cid := range sizes {
		chosen := selectDistinct(rng, nodes, R, strat)
		ids := make([]int, len(chosen))
		for i, n := range chosen {
			ids[i] = n.ID
			n.Pins[cid] = struct{}{}
			n.UsedBytes += sizes[cid]
		}
		pins[cid] = ids
	}
	return pins
}

// PinIncidenceByNode returns each node's replica count (len of Pins).
func PinIncidenceByNode(nodes []*Node) []float64 {
	out := make([]float64, len(nodes))
	for i, n := range nodes {
		out[i] = float64(len(n.Pins))
	}
	return out
}

// PinIncidenceBy buckets total replica incidence by a domain dimension
// (provider, ASN, region, principal), returning the per-bucket weights for
// the concentration metrics.
func PinIncidenceBy(nodes []*Node, key func(*Node) string) []float64 {
	buckets := map[string]float64{}
	for _, n := range nodes {
		buckets[key(n)] += float64(len(n.Pins))
	}
	out := make([]float64, 0, len(buckets))
	for _, v := range buckets {
		out = append(out, v)
	}
	return out
}

// Domain-dimension key helpers.
func KeyProvider(n *Node) string  { return n.Domain.Provider }
func KeyASN(n *Node) string       { return n.Domain.ASN }
func KeyRegion(n *Node) string    { return n.Domain.Region }
func KeyPrincipal(n *Node) string { return n.Domain.Principal }
