package handlers

import "testing"

// TestSettingsExposesBelowFloorKnobs verifies the P2-M7.1 first-class
// below_floor_replacement knobs (D-M7.1-3: "two /settings knobs: enabled,
// grace") carry EXPLICIT fieldEffect entries — first-class means an explicit
// entry (surfaced prominently in /settings), while the advanced tuning knobs
// (hysteresis_margin, requeue_batch) fall through to the restart default.
func TestSettingsExposesBelowFloorKnobs(t *testing.T) {
	for _, path := range []string{
		"below_floor_replacement.enabled",
		"below_floor_replacement.grace_seconds",
	} {
		if _, ok := fieldEffect[path]; !ok {
			t.Fatalf("first-class knob %q must have an explicit fieldEffect entry", path)
		}
		// All below_floor_replacement knobs are read at coordinator construction
		// (scheduler/commit-gate/pruner config) — restart-effect.
		if got := effectFor(path); got != effectRestart {
			t.Fatalf("effectFor(%q) = %v, want restart", path, got)
		}
	}
	// Advanced knobs: no explicit entry, restart via the unmatched default.
	for _, path := range []string{
		"below_floor_replacement.hysteresis_margin",
		"below_floor_replacement.requeue_batch",
	} {
		if _, ok := fieldEffect[path]; ok {
			t.Fatalf("advanced knob %q must NOT be first-class (no explicit entry)", path)
		}
		if got := effectFor(path); got != effectRestart {
			t.Fatalf("effectFor(%q) = %v, want restart (safe default)", path, got)
		}
	}
}
