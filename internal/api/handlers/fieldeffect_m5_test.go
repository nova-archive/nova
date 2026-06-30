package handlers

import "testing"

// TestSettingsExposesFirstClassKnobsOnly verifies the P2-M5 first-class healing
// knobs resolve to explicit effects (surfaced prominently in /settings) while the
// advanced orchestrator internals fall through to the inert section default.
func TestSettingsExposesFirstClassKnobsOnly(t *testing.T) {
	firstClass := map[string]effect{
		"orchestrator.replication.factor":            effectLive,    // reload hook recomputes targets
		"orchestrator.mass_casualty_threshold_ratio": effectRestart, // read at startup
		"orchestrator.capacity_runway_floor_days":    effectRestart,
		"orchestrator.reputation_floor":              effectRestart,
	}
	for path, want := range firstClass {
		if got := effectFor(path); got != want {
			t.Fatalf("effectFor(%q) = %v, want %v", path, got, want)
		}
	}
	// An advanced orchestrator knob has no explicit entry ⇒ inherits the inert
	// "orchestrator" section default (advanced/defaulted, not first-class).
	if got := effectFor("orchestrator.tick_interval_seconds"); got != effectInert {
		t.Fatalf("advanced knob effectFor = %v, want inert", got)
	}
}
