//go:build novasim

package model

import (
	"math/rand"
	"sort"
)

// DestinationPolicy chooses where a repair replica lands. The original sim
// used uniform-random non-holder selection; the Phase-2 design (D8) adds
// failure-domain anti-affinity. Keeping this separate from the initial
// PlacementStrategy mirrors reality: placement weight and repair-destination
// preference are distinct policies.
type DestinationPolicy interface {
	Name() string
	// Pick returns a surviving non-holder for cid, or nil if none exists.
	// holderNodes is the current holder set (for anti-affinity scoring).
	Pick(rng *rand.Rand, alive []*Node, isHolder func(id int) bool, holderNodes []*Node, size float64) *Node
}

// UniformDestination picks a uniform-random surviving non-holder (the
// orchestrator_resilience.py behaviour). Used for faithful cross-validation.
type UniformDestination struct{}

func (UniformDestination) Name() string { return "uniform" }

func (UniformDestination) Pick(rng *rand.Rand, alive []*Node, isHolder func(int) bool, _ []*Node, _ float64) *Node {
	for i := 0; i < 8; i++ {
		c := alive[rng.Intn(len(alive))]
		if !isHolder(c.ID) {
			return c
		}
	}
	for _, c := range alive { // rare fallback: linear scan
		if !isHolder(c.ID) {
			return c
		}
	}
	return nil
}

// AntiAffinityDestination samples a handful of surviving non-holders and
// returns the one whose DiversityOptimized weight (given current holders) is
// highest — soft failure-domain anti-affinity for repair placement.
type AntiAffinityDestination struct {
	Strategy DiversityOptimized
	Samples  int
}

// DefaultAntiAffinityDestination returns a 16-sample anti-affinity policy.
func DefaultAntiAffinityDestination() AntiAffinityDestination {
	return AntiAffinityDestination{Strategy: DefaultDiversityOptimized(), Samples: 16}
}

func (AntiAffinityDestination) Name() string { return "anti-affinity" }

func (d AntiAffinityDestination) Pick(rng *rand.Rand, alive []*Node, isHolder func(int) bool, holderNodes []*Node, _ float64) *Node {
	samples := d.Samples
	if samples < 1 {
		samples = 1
	}
	var best *Node
	var bestW float64 = -1
	for i := 0; i < samples; i++ {
		c := alive[rng.Intn(len(alive))]
		if isHolder(c.ID) {
			continue
		}
		w := d.Strategy.Weight(c, holderNodes)
		if w > bestW {
			best, bestW = c, w
		}
	}
	if best != nil {
		return best
	}
	// Fallback: any non-holder.
	for _, c := range alive {
		if !isHolder(c.ID) {
			return c
		}
	}
	return nil
}

// HealResult reports the outcome of a healing run. Times are simulated
// seconds; -1 means "not reached within max time".
type HealResult struct {
	// CIDsZeroHoldersInitial is the count of CIDs with no surviving LOCAL
	// holder right after the failure — the original sim's "CIDs lost forever".
	// With peer custodians these are recoverable, so it is an INITIAL snapshot.
	CIDsZeroHoldersInitial int
	// CIDsLostForever is the FINAL count of CIDs still at zero holders after
	// healing. Equals CIDsZeroHoldersInitial with no peers; driven toward zero
	// when peer custodians can reseed (proposed Phase 7).
	CIDsLostForever int

	InitialTier1      int
	InitialTier2      int
	Tier1ClearSeconds int // time to bring every healable CID to >= 2 holders; -1 if unfinished
	FullHealSeconds   int // time to bring every CID to target R; -1 if unfinished
	Tier1Remaining    int
	Tier2Remaining    int
	TotalEgressBytes  float64
	ElapsedSeconds    int
}

// HealConfig parameterises a healing run.
type HealConfig struct {
	TargetR     int
	StepSeconds int
	MaxSeconds  int
	Destination DestinationPolicy
	DestSeed    int64

	// PeerSources, if set, are always-surviving repair SOURCES (opaque
	// cross-federation custodians, proposed Phase 7). They can source any CID
	// and are paced by their own daily budget, but they NEVER become
	// destinations and NEVER count toward local pinCount / Tier classification
	// — peering replicates bytes, not local durability. Give peers IDs in a
	// disjoint range from the local nodes.
	PeerSources []*Node
}

// DefaultHealConfig returns the standard R=3, 60s-step, 14-day-cap run with
// uniform destination selection (cross-validation default).
func DefaultHealConfig() HealConfig {
	return HealConfig{
		TargetR:     3,
		StepSeconds: 60,
		MaxSeconds:  14 * 86400,
		Destination: UniformDestination{},
		DestSeed:    20269,
	}
}

// Heal runs the discrete-event healing simulation: strict Tier-1 priority,
// per-node step capacity = min(remaining daily budget, link × step), source =
// highest-step-capacity alive holder, destination per policy. Bandwidth
// budgets are never overridden (Tier-1 T1.12). It mutates node pin/budget
// state and the pins map; callers pass freshly-built state.
func Heal(nodes []*Node, pins [][]int, sizes []float64, cfg HealConfig) HealResult {
	rng := rand.New(rand.NewSource(cfg.DestSeed))

	holders := make([]map[int]struct{}, len(pins))
	pinCount := make([]int, len(pins))
	for cid, hs := range pins {
		live := make(map[int]struct{}, len(hs))
		for _, id := range hs {
			if nodes[id].Alive {
				live[id] = struct{}{}
			}
		}
		holders[cid] = live
		pinCount[cid] = len(live)
	}

	res := HealResult{Tier1ClearSeconds: -1, FullHealSeconds: -1}
	alive := make([]*Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Alive {
			alive = append(alive, n)
		}
	}
	// srcPool = local survivors ∪ peer custodians. Peers source and are
	// budget-paced but never destination, never counted toward durability.
	srcPool := make([]*Node, len(alive), len(alive)+len(cfg.PeerSources))
	copy(srcPool, alive)
	srcPool = append(srcPool, cfg.PeerSources...)
	srcByID := make(map[int]*Node, len(srcPool))
	for _, n := range srcPool {
		srcByID[n.ID] = n
	}
	hasPeers := len(cfg.PeerSources) > 0
	for cid := range pins {
		switch {
		case pinCount[cid] == 0:
			res.CIDsZeroHoldersInitial++
		case pinCount[cid] == 1:
			res.InitialTier1++
		case pinCount[cid] == 2:
			res.InitialTier2++
		}
	}
	if len(alive) == 0 {
		// No local survivor to receive a replica; nothing can heal.
		res.Tier1Remaining = res.CIDsZeroHoldersInitial + res.InitialTier1
		res.Tier2Remaining = res.InitialTier2
		res.CIDsLostForever = res.CIDsZeroHoldersInitial
		return res
	}

	// "critical" = every CID below 2 holders that is healable: a 1-holder CID
	// (a local source exists), or a 0-holder CID when peer custodians can
	// reseed it. 0-holder CIDs with no peers are terminally lost and excluded.
	// "tier2" = 2..R-1 holders. Both sorted small-file-first (cheap wins first).
	critical := collectQueue(pinCount, sizes, func(p int) bool { return p == 1 || (p == 0 && hasPeers) })
	tier2 := collectQueue(pinCount, sizes, func(p int) bool { return p > 1 && p < cfg.TargetR })
	if len(critical) == 0 {
		res.Tier1ClearSeconds = 0
	}
	if len(critical) == 0 && len(tier2) == 0 {
		res.FullHealSeconds = 0
		res.CIDsLostForever = countZeroHolders(pinCount)
		return res
	}
	tier1 := critical // the strict-priority queue below "tier1" now == critical

	stepCap := make(map[int]float64, len(alive))
	elapsed := 0
	nextReset := 86400

	holderNodes := func(cid int) []*Node {
		hs := holders[cid]
		out := make([]*Node, 0, len(hs))
		for id := range hs {
			out = append(out, nodes[id])
		}
		return out
	}

	attempt := func(cid int) bool {
		size := sizes[cid]
		// Source = the holder (local or peer custodian) with the most step
		// capacity that can cover size. Peers can source any CID.
		bestSrc := -1
		bestCap := 0.0
		consider := func(id int) {
			c := stepCap[id]
			if c >= size && c > bestCap {
				bestSrc, bestCap = id, c
			}
		}
		for id := range holders[cid] {
			consider(id)
		}
		for _, p := range cfg.PeerSources {
			consider(p.ID)
		}
		if bestSrc < 0 {
			return false
		}
		dest := cfg.Destination.Pick(rng, alive, func(id int) bool {
			_, ok := holders[cid][id]
			return ok
		}, holderNodes(cid), size)
		if dest == nil {
			return false
		}
		srcByID[bestSrc].BytesUploadedToday += size
		stepCap[bestSrc] -= size
		holders[cid][dest.ID] = struct{}{}
		dest.Pins[cid] = struct{}{}
		dest.UsedBytes += size
		pinCount[cid]++
		res.TotalEgressBytes += size
		return true
	}

	for elapsed < cfg.MaxSeconds {
		if elapsed >= nextReset {
			for _, n := range srcPool {
				n.BytesUploadedToday = 0
			}
			nextReset += 86400
		}
		maxCap := 0.0
		for _, n := range srcPool {
			c := n.RemainingBudgetBytes()
			if link := n.Profile.UploadBytesPerSec() * float64(cfg.StepSeconds); link < c {
				c = link
			}
			stepCap[n.ID] = c
			if c > maxCap {
				maxCap = c
			}
		}
		if maxCap <= 0 {
			elapsed = nextReset
			continue
		}

		progress := 0
		// Drain the critical queue strictly first. A single attempt adds one
		// holder, so a reseeded 0-holder CID becomes 1-holder and STAYS
		// critical until it reaches 2; a 1-holder CID promotes to tier2.
		var stillT1 []int
		for _, cid := range tier1 {
			if pinCount[cid] >= 2 {
				tier2 = append(tier2, cid)
				continue
			}
			if attempt(cid) {
				progress++
				if pinCount[cid] >= 2 {
					tier2 = append(tier2, cid)
				} else {
					stillT1 = append(stillT1, cid)
				}
			} else {
				stillT1 = append(stillT1, cid)
			}
		}
		tier1 = stillT1
		if len(tier1) == 0 && res.Tier1ClearSeconds < 0 {
			res.Tier1ClearSeconds = elapsed + cfg.StepSeconds
		}

		if len(tier1) == 0 {
			var stillT2 []int
			for _, cid := range tier2 {
				if pinCount[cid] >= cfg.TargetR {
					continue
				}
				if attempt(cid) {
					progress++
					if pinCount[cid] < cfg.TargetR {
						stillT2 = append(stillT2, cid)
					}
				} else {
					stillT2 = append(stillT2, cid)
				}
			}
			tier2 = stillT2
		}

		if len(tier1) == 0 && len(tier2) == 0 {
			res.FullHealSeconds = elapsed + cfg.StepSeconds
			break
		}
		if progress == 0 {
			elapsed = nextReset
			continue
		}
		elapsed += cfg.StepSeconds
	}

	res.Tier1Remaining = len(tier1)
	res.Tier2Remaining = len(tier2)
	res.ElapsedSeconds = elapsed
	res.CIDsLostForever = countZeroHolders(pinCount)
	return res
}

func countZeroHolders(pinCount []int) int {
	n := 0
	for _, p := range pinCount {
		if p == 0 {
			n++
		}
	}
	return n
}

func collectQueue(pinCount []int, sizes []float64, pred func(int) bool) []int {
	var q []int
	for cid, p := range pinCount {
		if pred(p) {
			q = append(q, cid)
		}
	}
	// Small-file-first.
	sortBySize(q, sizes)
	return q
}

func sortBySize(q []int, sizes []float64) {
	sort.Slice(q, func(a, b int) bool { return sizes[q[a]] < sizes[q[b]] })
}
