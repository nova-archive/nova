//go:build novasim

package model

import (
	"fmt"
	"math/rand"
)

// randFor returns a deterministic RNG seeded for failure application. Failure
// models draw from this; using a dedicated multiplier decorrelates the failure
// draw from network/placement seeds.
func randFor(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed * 104729)) }

// humanBytes renders a byte count with binary units.
func humanBytes(b float64) string {
	switch {
	case b >= TiB:
		return fmt.Sprintf("%.2f TiB", b/TiB)
	case b >= GiB:
		return fmt.Sprintf("%.2f GiB", b/GiB)
	case b >= MiB:
		return fmt.Sprintf("%.2f MiB", b/MiB)
	case b >= KiB:
		return fmt.Sprintf("%.2f KiB", b/KiB)
	default:
		return fmt.Sprintf("%.0f B", b)
	}
}

// humanDuration renders simulated seconds (or -1 for "not reached").
func humanDuration(seconds int) string {
	if seconds < 0 {
		return "—"
	}
	switch {
	case seconds < 600:
		return fmt.Sprintf("%d s", seconds)
	case seconds < 86400:
		return fmt.Sprintf("%d min", seconds/60)
	default:
		return fmt.Sprintf("%.1f days", float64(seconds)/86400.0)
	}
}
