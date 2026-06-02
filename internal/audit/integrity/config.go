// Package integrity implements Nova's coordinator-internal integrity audits
// (docs/specs/INTEGRITY_AUDIT.md): local-fixity checks over the coordinator's
// own Postgres state, the local Kubo blockstore, and the keystore. It catches
// implementation bugs and silent corruption before donors are involved.
//
// It is deliberately NOT donor-facing (no challenge tokens, no remote calls)
// and runs as an in-process Scheduler — it does not enqueue persistent jobs.
package integrity

import (
	"fmt"
	"time"
)

// Kind is one audit_kind enum value. The string values match the SQL
// audit_kind enum (docs/specs/DATA_MODEL.sql) exactly.
type Kind string

// The seven audit kinds defined by INTEGRITY_AUDIT.md § Scope.
const (
	KindEnvelopeDecode            Kind = "envelope_decode"
	KindKeyUnwrap                 Kind = "key_unwrap"
	KindSampleDecrypt             Kind = "sample_decrypt"
	KindKuboPinPresent            Kind = "kubo_pin_present"
	KindDerivativeStateConsistent Kind = "derivative_state_consistent"
	KindBlockHashValid            Kind = "block_hash_valid"
	KindManifestConsistent        Kind = "manifest_consistent"
)

// AllKinds is the canonical iteration order for scheduling and defaults.
var AllKinds = []Kind{
	KindEnvelopeDecode,
	KindKeyUnwrap,
	KindSampleDecrypt,
	KindKuboPinPresent,
	KindDerivativeStateConsistent,
	KindBlockHashValid,
	KindManifestConsistent,
}

// Cadence is a kind's schedule: how often it runs and how many rows it samples
// per run.
type Cadence struct {
	Interval   time.Duration
	SampleSize int
}

// DefaultCadences returns the INTEGRITY_AUDIT.md § "Schedule" defaults. These
// ship as code constants; operator.yaml decode of the integrity_audit section
// is deferred (M5–M7 precedent).
func DefaultCadences() map[Kind]Cadence {
	return map[Kind]Cadence{
		KindEnvelopeDecode:            {Interval: time.Hour, SampleSize: 100},
		KindKeyUnwrap:                 {Interval: time.Hour, SampleSize: 100},
		KindSampleDecrypt:             {Interval: time.Hour, SampleSize: 50},
		KindKuboPinPresent:            {Interval: 15 * time.Minute, SampleSize: 200},
		KindDerivativeStateConsistent: {Interval: time.Hour, SampleSize: 200},
		KindBlockHashValid:            {Interval: 24 * time.Hour, SampleSize: 100},
		KindManifestConsistent:        {Interval: 24 * time.Hour, SampleSize: 100},
	}
}

// sampleBounds are the spec's sane sample-size limits (INTEGRITY_AUDIT.md §
// Schedule): "1..10000".
const (
	minSampleSize = 1
	maxSampleSize = 10000
)

// checkSampleSizes validates every cadence's sample size is within bounds. It
// is shared by the prod and dev EnforceAuditPolicy variants.
func checkSampleSizes(c map[Kind]Cadence) error {
	for k, cad := range c {
		if cad.SampleSize < minSampleSize || cad.SampleSize > maxSampleSize {
			return fmt.Errorf("integrity: kind %q sample size %d out of bounds [%d,%d]",
				k, cad.SampleSize, minSampleSize, maxSampleSize)
		}
	}
	return nil
}
