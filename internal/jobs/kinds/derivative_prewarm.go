// Package kinds holds the job-kind constants and handlers registered with the
// worker pool.
package kinds

import (
	"context"
	"encoding/json"

	"github.com/nova-archive/nova/internal/jobs"
)

const KindDerivativePrewarm = "derivative_prewarm"

type DerivativePrewarmPayload struct {
	ParentCID string   `json:"parent_cid"`
	Presets   []string `json:"presets"`
}

// NewDerivativePrewarmHandler builds the handler that pre-generates presets for
// a freshly-committed image. prewarm is the nova-image generation fn (best-effort:
// it should log per-preset failures and return an error only when the parent is
// unreadable, so the queue's backoff retries a transient failure).
func NewDerivativePrewarmHandler(prewarm func(ctx context.Context, parentCID string, presets []string) error) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p DerivativePrewarmPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return err
		}
		return prewarm(ctx, p.ParentCID, p.Presets)
	}
}
