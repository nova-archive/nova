//go:build nova_dev

package integrity

// EnforceAuditPolicy validates a cadence map for a dev build. A zero interval
// is permitted (it disables that kind — useful in tests and local runs); only
// the sample-size bounds are enforced.
func EnforceAuditPolicy(c map[Kind]Cadence) error {
	return checkSampleSizes(c)
}
