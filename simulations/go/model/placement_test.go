//go:build novasim

package model

import (
	"math/rand"
	"testing"
)

func TestAssignStorageDistinctHolders(t *testing.T) {
	nodes := BuildNetwork(DefaultNetworkConfig(50), 1)
	sizes := FileSizes(2000, 0.5*MiB, 1)
	pins := AssignStorage(nodes, sizes, 3, BandwidthWeighted{}, 1)
	for cid, hs := range pins {
		if len(hs) != 3 {
			t.Fatalf("cid %d: got %d holders, want 3", cid, len(hs))
		}
		seen := map[int]bool{}
		for _, id := range hs {
			if seen[id] {
				t.Fatalf("cid %d: duplicate holder %d", cid, id)
			}
			seen[id] = true
		}
	}
}

// TestDiversityReducesConcentration is the unit-test form of ChatGPT's
// Priority-2 claim: decoupling placement weight from bandwidth should spread
// replicas across more providers, lowering provider-level concentration.
func TestDiversityReducesConcentration(t *testing.T) {
	cfg := DefaultNetworkConfig(200)
	const cids = 20000

	bw := BuildNetwork(cfg, 7)
	AssignStorage(bw, FileSizes(cids, 0.5*MiB, 7), 3, BandwidthWeighted{}, 7)
	bwProvider := Concentration(PinIncidenceBy(bw, KeyProvider))

	div := BuildNetwork(cfg, 7)
	AssignStorage(div, FileSizes(cids, 0.5*MiB, 7), 3, DefaultDiversityOptimized(), 7)
	divProvider := Concentration(PinIncidenceBy(div, KeyProvider))

	t.Logf("provider Gini: bandwidth-weighted=%.3f diversity-optimized=%.3f",
		bwProvider.Gini, divProvider.Gini)
	t.Logf("provider largest-share: bandwidth-weighted=%.3f diversity-optimized=%.3f",
		bwProvider.LargestShare, divProvider.LargestShare)

	if divProvider.Gini >= bwProvider.Gini {
		t.Errorf("diversity-optimized provider Gini (%.3f) should be below bandwidth-weighted (%.3f)",
			divProvider.Gini, bwProvider.Gini)
	}
	if divProvider.LargestShare >= bwProvider.LargestShare {
		t.Errorf("diversity-optimized largest-provider share (%.3f) should be below bandwidth-weighted (%.3f)",
			divProvider.LargestShare, bwProvider.LargestShare)
	}
}

func TestAntiAffinityPrefersDifferentProvider(t *testing.T) {
	// One holder on provider A; two candidates: same provider A, different B.
	a := newNode(0, HighBandwidthVPS, Domain{Provider: "A", ASN: "A", Region: "R", Principal: "p0", Host: "h0"})
	sameProv := newNode(1, HighBandwidthVPS, Domain{Provider: "A", ASN: "A", Region: "R", Principal: "p1", Host: "h1"})
	diffProv := newNode(2, HighBandwidthVPS, Domain{Provider: "B", ASN: "B", Region: "R2", Principal: "p2", Host: "h2"})
	alive := []*Node{sameProv, diffProv}
	holders := []*Node{a}

	pol := DefaultAntiAffinityDestination()
	pol.Samples = 64
	rng := rand.New(rand.NewSource(3))
	diffCount := 0
	for i := 0; i < 200; i++ {
		got := pol.Pick(rng, alive, func(id int) bool { return id == 0 }, holders, 1*MiB)
		if got == nil {
			t.Fatal("expected a destination")
		}
		if got.ID == diffProv.ID {
			diffCount++
		}
	}
	if diffCount < 180 {
		t.Errorf("anti-affinity picked the different-provider node only %d/200 times; expected a strong majority", diffCount)
	}
}
