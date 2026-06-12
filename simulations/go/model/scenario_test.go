//go:build novasim

package model

import "testing"

func TestDefaultScenarioRuns(t *testing.T) {
	res := RunScenario(DefaultScenario(60))
	if res.AliveNodes <= 0 || res.AliveNodes >= res.NumNodes {
		t.Errorf("AliveNodes = %d (of %d), want a partial survival", res.AliveNodes, res.NumNodes)
	}
	if res.Heal.Tier1ClearSeconds < 0 {
		t.Errorf("Tier-1 did not clear at 60 nodes (remaining=%d)", res.Heal.Tier1Remaining)
	}
	if res.TotalDataBytes <= 0 {
		t.Error("TotalDataBytes should be positive")
	}
}

// TestConcentrationAlertingDistinguishesStrategies shows the "alert, not
// prevent" signal works: bandwidth-weighted placement trips concentration
// alerts that diversity-optimized placement avoids — without either strategy
// ever refusing to place.
func TestConcentrationAlertingDistinguishesStrategies(t *testing.T) {
	base := DefaultScenario(200)
	base.NumCIDs = 20000
	base.Failure = UniformFailure{Rate: 0.0} // isolate steady-state concentration

	bw := base
	bw.Placement = BandwidthWeighted{}
	bwAlerts := DefaultAlertThresholds().Evaluate(RunScenario(bw))

	div := base
	div.Placement = DefaultDiversityOptimized()
	divAlerts := DefaultAlertThresholds().Evaluate(RunScenario(div))

	t.Logf("bandwidth-weighted fired %d concentration alert(s); diversity-optimized fired %d",
		len(bwAlerts), len(divAlerts))
	for _, a := range bwAlerts {
		t.Logf("  bw alert: %s %s=%.3f (>%.2f)", a.Dimension, a.Metric, a.Value, a.Threshold)
	}
	if len(bwAlerts) <= len(divAlerts) {
		t.Errorf("bandwidth-weighted (%d alerts) should trip more than diversity-optimized (%d)",
			len(bwAlerts), len(divAlerts))
	}
}
