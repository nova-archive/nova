//go:build !nova_dev

package integrity

import "fmt"

// EnforceAuditPolicy validates a cadence map for a production build. A zero (or
// negative) interval disables a kind, which INTEGRITY_AUDIT.md permits only
// under the nova_dev build tag — so production builds refuse it. Sample sizes
// must be within the spec's [1,10000] bounds. Mirrors auth.EnforceAnonymousPolicy.
func EnforceAuditPolicy(c map[Kind]Cadence) error {
	for k, cad := range c {
		if cad.Interval <= 0 {
			return fmt.Errorf("integrity: kind %q has interval<=0 (disabled); refused in production builds", k)
		}
	}
	return checkSampleSizes(c)
}
