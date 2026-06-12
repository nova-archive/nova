//go:build novasim

package model

import (
	"fmt"
	"math"
	"math/rand"
)

// Byte-size units. The model works in explicit bytes throughout.
const (
	KiB = 1 << 10
	MiB = 1 << 20
	GiB = 1 << 30
	TiB = 1 << 40
)

// Profile is a donor node's resource class. BudgetBytesPerDay is the
// operator-set daily egress budget the healing engine must never exceed
// (Tier-1 T1.12); LinkMbps is the physical upload-link cap in decimal
// megabits/sec.
type Profile struct {
	Name              string
	BudgetBytesPerDay float64
	LinkMbps          float64
	CapBytes          float64 // total storage capacity (for free-space placement weighting)
}

// UploadBytesPerSec converts the decimal-megabit link cap to bytes/sec.
func (p Profile) UploadBytesPerSec() float64 { return p.LinkMbps * 1e6 / 8.0 }

// The two donor classes mirror simulations/orchestrator_resilience.py: a
// high-bandwidth VPS class and a residential class. The 40.96× budget ratio
// (2 TiB vs 50 GiB per day) is exactly what drives capacity-weighted
// placement to concentrate replicas — see the design doc.
var (
	HighBandwidthVPS = Profile{
		Name:              "high-bandwidth-vps",
		BudgetBytesPerDay: 2 * TiB,
		LinkMbps:          1000,
		CapBytes:          4 * TiB,
	}
	Residential = Profile{
		Name:              "residential",
		BudgetBytesPerDay: 50 * GiB,
		LinkMbps:          50,
		CapBytes:          200 * GiB,
	}
)

// Domain holds a node's failure-domain coordinates. Correlated failure
// targets one field across many nodes (a provider purge, an ASN partition,
// a region outage, a Sybil principal withdrawal). Two nodes are independent
// only to the extent their Domain fields differ.
type Domain struct {
	Principal string // donor operator identity; the Sybil / multi-account unit
	Provider  string // hosting provider account
	ASN       string // autonomous system
	Region    string // datacenter region / metro
	Host      string // physical host
}

// Node is a donor.
type Node struct {
	ID                 int
	Profile            Profile
	Domain             Domain
	Alive              bool
	Trust              float64 // placement-weight multiplier in (0,1]; probationary < trusted
	Pins               map[int]struct{}
	BytesUploadedToday float64
	UsedBytes          float64
}

func newNode(id int, p Profile, d Domain) *Node {
	return &Node{
		ID:      id,
		Profile: p,
		Domain:  d,
		Alive:   true,
		Trust:   1.0,
		Pins:    make(map[int]struct{}),
	}
}

// RemainingBudgetBytes is the node's unused daily egress budget.
func (n *Node) RemainingBudgetBytes() float64 {
	r := n.Profile.BudgetBytesPerDay - n.BytesUploadedToday
	if r < 0 {
		return 0
	}
	return r
}

// FreeBytes is the node's unused storage capacity.
func (n *Node) FreeBytes() float64 {
	f := n.Profile.CapBytes - n.UsedBytes
	if f < 0 {
		return 0
	}
	return f
}

// NetworkConfig parameterises a synthetic federation. The defaults model a
// realistic, mildly-concentrated donor population: VPS nodes cluster onto a
// handful of big providers (the homogeneity an operator should be alerted
// about), residential nodes spread across many consumer ISPs.
type NetworkConfig struct {
	NumNodes              int
	HighBandwidthVPSRatio float64 // default 0.15
	NumVPSProviders       int     // VPS providers the VPS cohort draws from
	NumResidentialASNs    int     // consumer ISPs the residential cohort spreads across
	NumRegions            int
	// VPSProviderSkew is the Zipf-like exponent for how unevenly VPS nodes
	// distribute across VPS providers. 0 = uniform; higher = a few providers
	// dominate (realistic). Default 1.0.
	VPSProviderSkew float64
}

// DefaultNetworkConfig returns the standard population for N nodes.
func DefaultNetworkConfig(numNodes int) NetworkConfig {
	return NetworkConfig{
		NumNodes:              numNodes,
		HighBandwidthVPSRatio: 0.15,
		NumVPSProviders:       8,
		NumResidentialASNs:    40,
		NumRegions:            12,
		VPSProviderSkew:       1.0,
	}
}

// BuildNetwork constructs a synthetic donor population. Each node gets a
// profile and a Domain. By default Principal and Host are unique per node
// (no Sybil concentration); Provider/ASN/Region are shared across cohorts to
// create realistic correlated-failure structure.
func BuildNetwork(cfg NetworkConfig, seed int64) []*Node {
	rng := rand.New(rand.NewSource(seed * 7919))
	if cfg.NumVPSProviders < 1 {
		cfg.NumVPSProviders = 1
	}
	if cfg.NumResidentialASNs < 1 {
		cfg.NumResidentialASNs = 1
	}
	if cfg.NumRegions < 1 {
		cfg.NumRegions = 1
	}

	// Zipf weights over VPS providers so a few dominate.
	vpsWeights := make([]float64, cfg.NumVPSProviders)
	for i := range vpsWeights {
		vpsWeights[i] = 1.0 / math.Pow(float64(i+1), cfg.VPSProviderSkew)
	}
	regionFor := func() string { return fmt.Sprintf("region-%d", rng.Intn(cfg.NumRegions)) }

	// Stable region per provider/ASN so a "region outage" hits coherent sets.
	vpsRegion := make([]string, cfg.NumVPSProviders)
	for i := range vpsRegion {
		vpsRegion[i] = regionFor()
	}
	ispRegion := make([]string, cfg.NumResidentialASNs)
	for i := range ispRegion {
		ispRegion[i] = regionFor()
	}

	nodes := make([]*Node, 0, cfg.NumNodes)
	for id := 0; id < cfg.NumNodes; id++ {
		var d Domain
		var p Profile
		if rng.Float64() < cfg.HighBandwidthVPSRatio {
			p = HighBandwidthVPS
			pi := weightedIndex(rng, vpsWeights)
			d = Domain{
				Provider: fmt.Sprintf("vps-provider-%d", pi),
				ASN:      fmt.Sprintf("AS-vps-%d", pi),
				Region:   vpsRegion[pi],
			}
		} else {
			p = Residential
			ai := rng.Intn(cfg.NumResidentialASNs)
			d = Domain{
				Provider: fmt.Sprintf("isp-%d", ai),
				ASN:      fmt.Sprintf("AS-isp-%d", ai),
				Region:   ispRegion[ai],
			}
		}
		d.Principal = fmt.Sprintf("principal-%d", id) // unique by default
		d.Host = fmt.Sprintf("host-%d", id)           // unique by default
		nodes = append(nodes, newNode(id, p, d))
	}
	return nodes
}

// weightedIndex draws an index in [0,len(weights)) proportional to weights.
func weightedIndex(rng *rand.Rand, weights []float64) int {
	var total float64
	for _, w := range weights {
		total += w
	}
	if total <= 0 {
		return rng.Intn(len(weights))
	}
	pick := rng.Float64() * total
	var cum float64
	for i, w := range weights {
		cum += w
		if cum >= pick {
			return i
		}
	}
	return len(weights) - 1
}

// CountAlive returns the number of live nodes.
func CountAlive(nodes []*Node) int {
	n := 0
	for _, nd := range nodes {
		if nd.Alive {
			n++
		}
	}
	return n
}
