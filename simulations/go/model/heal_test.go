//go:build novasim

package model

import (
	"math"
	"testing"
)

func TestHealClearsTier1OnHealthyNetwork(t *testing.T) {
	nodes := BuildNetwork(DefaultNetworkConfig(60), 2)
	sizes := FileSizes(20000, 0.5*MiB, 2)
	pins := AssignStorage(nodes, sizes, 3, BandwidthWeighted{}, 2)
	UniformFailure{Rate: 0.40}.Apply(nodes, randFor(2))

	res := Heal(nodes, pins, sizes, DefaultHealConfig())

	if res.Tier1ClearSeconds < 0 {
		t.Errorf("Tier-1 never cleared on a 60-node network (remaining=%d)", res.Tier1Remaining)
	}
	if res.Tier1Remaining != 0 {
		t.Errorf("Tier1Remaining = %d, want 0", res.Tier1Remaining)
	}
	// Tier-1 must clear no later than full heal (strict priority).
	if res.FullHealSeconds >= 0 && res.Tier1ClearSeconds > res.FullHealSeconds {
		t.Errorf("Tier-1 cleared (%ds) after full heal (%ds) — priority violated",
			res.Tier1ClearSeconds, res.FullHealSeconds)
	}
}

func TestCIDsLostForeverWhenAllHoldersDie(t *testing.T) {
	// 4 nodes, R=2, 1 CID. Kill exactly the two holders.
	nodes := []*Node{
		newNode(0, Residential, Domain{Principal: "p0", Host: "h0"}),
		newNode(1, Residential, Domain{Principal: "p1", Host: "h1"}),
		newNode(2, Residential, Domain{Principal: "p2", Host: "h2"}),
		newNode(3, Residential, Domain{Principal: "p3", Host: "h3"}),
	}
	sizes := []float64{1 * MiB}
	pins := [][]int{{0, 1}} // CID 0 held by nodes 0 and 1
	nodes[0].Pins[0] = struct{}{}
	nodes[1].Pins[0] = struct{}{}
	nodes[0].Alive = false
	nodes[1].Alive = false

	res := Heal(nodes, pins, sizes, DefaultHealConfig())
	if res.CIDsLostForever != 1 {
		t.Errorf("CIDsLostForever = %d, want 1", res.CIDsLostForever)
	}
}

// TestBudgetCapNeverExceededInOneDay is the airtight budget-enforcement proof:
// a lone survivor with a payload far larger than its daily budget can never
// upload more than that budget within a single day (no reset reached), no
// matter how much Tier-1 work is pending. This is the inviolable-budget
// invariant (Tier-1 T1.12).
func TestBudgetCapNeverExceededInOneDay(t *testing.T) {
	const cids = 8000 // ~312 GiB at 40 MiB each — ~6× the daily budget
	nodes := []*Node{
		newNode(0, Residential, Domain{Principal: "p0", Host: "h0", Provider: "A", ASN: "A", Region: "R"}),
		newNode(1, Residential, Domain{Principal: "p1", Host: "h1", Provider: "B", ASN: "B", Region: "R"}),
		newNode(2, Residential, Domain{Principal: "p2", Host: "h2", Provider: "C", ASN: "C", Region: "R"}),
	}
	sizes := make([]float64, cids)
	pins := make([][]int, cids)
	for c := range sizes {
		sizes[c] = 40 * MiB
		pins[c] = []int{0, 1}
		nodes[0].Pins[c] = struct{}{}
		nodes[1].Pins[c] = struct{}{}
	}
	nodes[1].Alive = false

	cfg := DefaultHealConfig()
	cfg.MaxSeconds = 86400 // exactly one budget day; no reset occurs
	res := Heal(nodes, pins, sizes, cfg)

	budget := nodes[0].Profile.BudgetBytesPerDay
	if res.TotalEgressBytes > budget {
		t.Errorf("survivor uploaded %s in one day, exceeding the %s daily budget (override!)",
			humanBytes(res.TotalEgressBytes), humanBytes(budget))
	}
	t.Logf("one-day egress %s vs %s budget (Tier-1 still %d unfinished — paced, not overridden)",
		humanBytes(res.TotalEgressBytes), humanBytes(budget), res.Tier1Remaining)
}

// TestBudgetPacingTakesMultipleDays illustrates the pacing story for the
// design doc: a 50 GiB/day survivor re-replicating ~156 GiB needs ~3 days —
// budget-paced, not instant. The lower bound is floor(payload/budget) days
// because the first budget day is available from t=0.
func TestBudgetPacingTakesMultipleDays(t *testing.T) {
	const cids = 4000
	nodes := []*Node{
		newNode(0, Residential, Domain{Principal: "p0", Host: "h0", Provider: "A", ASN: "A", Region: "R"}),
		newNode(1, Residential, Domain{Principal: "p1", Host: "h1", Provider: "B", ASN: "B", Region: "R"}),
		newNode(2, Residential, Domain{Principal: "p2", Host: "h2", Provider: "C", ASN: "C", Region: "R"}),
	}
	sizes := make([]float64, cids)
	pins := make([][]int, cids)
	for c := range sizes {
		sizes[c] = 40 * MiB
		pins[c] = []int{0, 1}
		nodes[0].Pins[c] = struct{}{}
		nodes[1].Pins[c] = struct{}{}
	}
	nodes[1].Alive = false

	payload := float64(cids) * 40 * MiB // ~156 GiB
	budget := nodes[0].Profile.BudgetBytesPerDay
	res := Heal(nodes, pins, sizes, DefaultHealConfig())

	if res.Tier1Remaining != 0 {
		t.Fatalf("Tier-1 did not clear within cap (remaining=%d)", res.Tier1Remaining)
	}
	// Conservation: total egress equals the payload (one transfer per CID).
	if math.Abs(res.TotalEgressBytes-payload) > 1*MiB {
		t.Errorf("egress %s != payload %s (conservation broken)", humanBytes(res.TotalEgressBytes), humanBytes(payload))
	}
	gotDays := float64(res.Tier1ClearSeconds) / 86400.0
	floorDays := math.Floor(payload / budget) // ~3; first budget day starts at t=0
	if gotDays < floorDays-0.05 {
		t.Errorf("Tier-1 cleared in %.2f days, below the %.0f-day budget floor (override?)", gotDays, floorDays)
	}
	if gotDays < 2.0 {
		t.Errorf("Tier-1 cleared in %.2f days — implausibly fast for a budget-paced single survivor", gotDays)
	}
	t.Logf("survivor paced %s through a %s/day budget in %.2f days (floor %.0f days)",
		humanBytes(payload), humanBytes(budget), gotDays, floorDays)
}
