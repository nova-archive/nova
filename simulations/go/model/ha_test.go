//go:build novasim

package model

import (
	"math"
	"testing"
)

func TestCoordinatorTierAvailability(t *testing.T) {
	const a = 0.99
	// k=1 returns a regardless of rho.
	approx(t, CoordinatorTierAvailability(a, 1, 0), a, 1e-12, "k=1,rho=0")
	approx(t, CoordinatorTierAvailability(a, 1, 1), a, 1e-12, "k=1,rho=1")
	// Uncorrelated k=3: 1-(1-a)^3.
	approx(t, CoordinatorTierAvailability(a, 3, 0), 1-math.Pow(1-a, 3), 1e-12, "k=3,rho=0")
	// Fully correlated collapses to a.
	approx(t, CoordinatorTierAvailability(a, 3, 1), a, 1e-12, "k=3,rho=1")
	// More replicas never reduce availability (rho<1).
	if CoordinatorTierAvailability(a, 3, 0.3) <= CoordinatorTierAvailability(a, 1, 0.3) {
		t.Error("3 coordinators should beat 1 at rho=0.3")
	}
}

func TestReadAvailabilityFloorDominatedBySharedHubs(t *testing.T) {
	// Three coordinators at 0.99 push the coordinator tier to ~0.999999, but a
	// single shared Postgres at 0.995 caps the end-to-end read availability.
	coord := CoordinatorTierAvailability(0.99, 3, 0.0)
	withHA := ReadAvailability(coord, 0.995, 0.9999, 0.9995)
	if withHA >= 0.995 {
		t.Errorf("end-to-end read availability %.6f should be capped below the 0.995 DB floor", withHA)
	}
	t.Logf("coord tier=%.6f, end-to-end read=%.6f (%.0f h/yr downtime) — DB/key/ingress is the floor",
		coord, withHA, Downtime(withHA))
}

// TestPeerSourcesRescueOtherwiseLostCIDs is the Phase-7 recovery-delta
// experiment. Capacity-weighted placement concentrates replicas on the VPS
// cohort, so a provider purge of that cohort drops many CIDs to zero LOCAL
// holders — terminally lost in today's single-federation design. Opaque peer
// custodians convert those losses into recoveries: peering replicates bytes,
// not authority.
func TestPeerSourcesRescueOtherwiseLostCIDs(t *testing.T) {
	build := func() ([]*Node, [][]int, []float64) {
		nodes := BuildNetwork(DefaultNetworkConfig(60), 5)
		sizes := FileSizes(20000, 0.5*MiB, 5)
		pins := AssignStorage(nodes, sizes, 3, BandwidthWeighted{}, 5)
		// Purge every VPS provider (ranked by budget) -> residential survivors.
		LargestDomainPurge{KeyName: "provider", Key: KeyProvider, NumBuckets: 8}.Apply(nodes, randFor(5))
		return nodes, pins, sizes
	}

	n1, p1, s1 := build()
	withoutPeers := Heal(n1, p1, s1, DefaultHealConfig())

	n2, p2, s2 := build()
	cfg := DefaultHealConfig()
	cfg.PeerSources = BuildPeerSources(2, HighBandwidthVPS, 100000)
	withPeers := Heal(n2, p2, s2, cfg)

	t.Logf("provider purge: zero-holders initial=%d | lost-forever without peers=%d, with 2 peers=%d",
		withoutPeers.CIDsZeroHoldersInitial, withoutPeers.CIDsLostForever, withPeers.CIDsLostForever)

	if withoutPeers.CIDsLostForever == 0 {
		t.Skip("purge caused no permanent local loss; nothing for peers to rescue")
	}
	if withPeers.CIDsLostForever >= withoutPeers.CIDsLostForever {
		t.Errorf("peers should rescue otherwise-lost CIDs: with=%d without=%d",
			withPeers.CIDsLostForever, withoutPeers.CIDsLostForever)
	}
}
