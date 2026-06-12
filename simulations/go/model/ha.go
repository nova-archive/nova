//go:build novasim

package model

import (
	"math"
	"strconv"
)

// SeriesAvailability composes independent stages in series (read path =
// ingress AND coordinator AND database AND keys AND a ciphertext source). The
// product is dominated by its weakest stage, which is why adding coordinator
// replicas alone cannot lift end-to-end availability past the shared DB / key
// / ingress floor.
func SeriesAvailability(stages ...float64) float64 {
	p := 1.0
	for _, s := range stages {
		p *= s
	}
	return p
}

// CoordinatorTierAvailability returns the availability of a k-coordinator
// serving tier where each coordinator has availability a. rho in [0,1] is the
// fraction of failures that are correlated (shared ingress, provider, region,
// power). rho=0 gives the fully-independent 1-(1-a)^k; rho=1 collapses to a
// single coordinator's a (replicas share fate). This is the central caveat to
// naive "multiply the nines" reasoning.
func CoordinatorTierAvailability(a float64, k int, rho float64) float64 {
	if k < 1 {
		k = 1
	}
	independent := 1 - math.Pow(1-a, float64(k))
	correlated := a
	return rho*correlated + (1-rho)*independent
}

// ReadAvailability composes the coordinator tier with the irreducible shared
// hubs (database, key material, ingress/DNS). Pass the coordinator tier from
// CoordinatorTierAvailability.
func ReadAvailability(coordTier, dbA, keyA, ingressA float64) float64 {
	return SeriesAvailability(coordTier, dbA, keyA, ingressA)
}

// Downtime converts an availability to annual downtime in hours.
func Downtime(a float64) float64 { return (1 - a) * 365.25 * 24 }

// BuildPeerSources constructs numPeers opaque cross-federation custodians for
// use as Heal repair sources (proposed Phase 7). Each peer is one independent
// failure domain with its own egress budget; the profile sets a peer's
// aggregate repair-serving capacity. IDs start at startID and must be disjoint
// from the local node ID space.
func BuildPeerSources(numPeers int, profile Profile, startID int) []*Node {
	peers := make([]*Node, 0, numPeers)
	for i := 0; i < numPeers; i++ {
		id := startID + i
		s := strconv.Itoa(i)
		d := Domain{
			Principal: "peer-fed-" + s,
			Provider:  "peer-fed-" + s,
			ASN:       "AS-peer-" + s,
			Region:    "peer-region-" + s,
			Host:      "peer-host-" + s,
		}
		peers = append(peers, newNode(id, profile, d))
	}
	return peers
}
