// Package kinds holds the job-kind constants and handlers registered with the
// worker pool. derivative_prewarm is a no-op in Phase 1 M4; nova-image wires
// the real body and the enqueue site in M5 (the product OnCommitted hook).
package kinds

import "context"

// KindDerivativePrewarm pre-warms common image-derivative presets after a
// parent upload commits. M4 ships the kind so M5 only has to fill the body.
const KindDerivativePrewarm = "derivative_prewarm"

// DerivativePrewarmStub is the M4 no-op handler. It satisfies jobs.Handler.
func DerivativePrewarmStub(ctx context.Context, payload []byte) error { return nil }
