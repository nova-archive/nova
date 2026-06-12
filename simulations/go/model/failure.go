//go:build novasim

package model

import (
	"math/rand"
	"sort"
)

// FailureModel removes a set of nodes from the network, returning the indices
// it killed. Models range from uncorrelated (uniform) to fully correlated
// (an entire provider / ASN / region / principal disappearing at once) — the
// "unit of failure is the failure domain, not the node" thesis.
type FailureModel interface {
	Name() string
	Apply(nodes []*Node, rng *rand.Rand) []int
}

func killAll(nodes []*Node, ids []int) {
	for _, i := range ids {
		nodes[i].Alive = false
	}
}

// UniformFailure vaporises Rate fraction of nodes at random — a regional
// hardware outage uncorrelated with profile. The best-case lower bound.
type UniformFailure struct{ Rate float64 }

func (UniformFailure) Name() string { return "uniform" }

func (f UniformFailure) Apply(nodes []*Node, rng *rand.Rand) []int {
	idx := rng.Perm(len(nodes))
	n := int(float64(len(nodes)) * f.Rate)
	dead := idx[:n]
	killAll(nodes, dead)
	return dead
}

// ProfileBiasFailure vaporises Rate fraction of nodes, preferring Profile
// first — the original "single provider purges the high-bandwidth-VPS cohort"
// model from orchestrator_resilience.py (--profile-bias).
type ProfileBiasFailure struct {
	Rate    float64
	Profile string
}

func (ProfileBiasFailure) Name() string { return "profile-bias" }

func (f ProfileBiasFailure) Apply(nodes []*Node, rng *rand.Rand) []int {
	order := rng.Perm(len(nodes))
	// Stable partition: preferred-profile nodes first, keeping the random
	// order within each class.
	sort.SliceStable(order, func(a, b int) bool {
		ai := nodes[order[a]].Profile.Name == f.Profile
		bi := nodes[order[b]].Profile.Name == f.Profile
		if ai == bi {
			return false
		}
		return ai
	})
	n := int(float64(len(nodes)) * f.Rate)
	dead := order[:n]
	killAll(nodes, dead)
	return dead
}

// LargestDomainPurge kills every node in the NumBuckets largest buckets of a
// failure-domain dimension, ranked by aggregate daily budget (capacity). This
// is the correlated-failure scenario the original sim could not express:
// "lose your largest provider / ASN / region outright." KeyName is for
// reporting; Key extracts the bucket id.
type LargestDomainPurge struct {
	KeyName    string
	Key        func(*Node) string
	NumBuckets int
}

func (f LargestDomainPurge) Name() string { return "largest-" + f.KeyName + "-purge" }

func (f LargestDomainPurge) Apply(nodes []*Node, rng *rand.Rand) []int {
	budget := map[string]float64{}
	members := map[string][]int{}
	for i, n := range nodes {
		k := f.Key(n)
		budget[k] += n.Profile.BudgetBytesPerDay
		members[k] = append(members[k], i)
	}
	type bucket struct {
		id  string
		cap float64
	}
	buckets := make([]bucket, 0, len(budget))
	for id, c := range budget {
		buckets = append(buckets, bucket{id, c})
	}
	sort.Slice(buckets, func(a, b int) bool { return buckets[a].cap > buckets[b].cap })

	nb := f.NumBuckets
	if nb < 1 {
		nb = 1
	}
	if nb > len(buckets) {
		nb = len(buckets)
	}
	var dead []int
	for i := 0; i < nb; i++ {
		dead = append(dead, members[buckets[i].id]...)
	}
	killAll(nodes, dead)
	return dead
}

// PrincipalWithdrawal kills every node belonging to the given principals — the
// Sybil "coordinated simultaneous withdrawal" model, now domain-aware (a
// principal may span many apparent providers/accounts).
type PrincipalWithdrawal struct{ Principals map[string]bool }

func (PrincipalWithdrawal) Name() string { return "principal-withdrawal" }

func (f PrincipalWithdrawal) Apply(nodes []*Node, rng *rand.Rand) []int {
	var dead []int
	for i, n := range nodes {
		if f.Principals[n.Domain.Principal] {
			dead = append(dead, i)
		}
	}
	killAll(nodes, dead)
	return dead
}
