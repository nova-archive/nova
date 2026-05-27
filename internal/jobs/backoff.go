// Package jobs is Nova's Postgres-backed job queue. Workers across the
// coordinator's milestones (integrity audits, scheduled tombstones,
// derivative prewarming, master-key rotation, webhook emission) all
// consume from this queue. The queue is partitioned by created_at
// monthly; see migration 0002_jobs.sql.
package jobs

import "time"

// Backoff returns the delay to apply before a job becomes eligible
// after a retryable failure. Exponential growth (5s, 10s, 20s, ...)
// capped at 5 minutes.
//
// Attempts is the count after the current failure (i.e., the row's
// attempts column post-increment). Attempts=0 returns the base delay
// so first-retry latency is consistent with subsequent retries.
func Backoff(attempts int) time.Duration {
	const base = 5 * time.Second
	const cap = 5 * time.Minute

	delay := base
	for i := 0; i < attempts-1; i++ {
		delay *= 2
		if delay > cap {
			return cap
		}
	}
	if delay > cap {
		return cap
	}
	return delay
}
