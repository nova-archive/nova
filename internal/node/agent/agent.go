// Package agent runs the donor's register→heartbeat→sync loop. In M1 the loop
// is a NO-OP: there is no transport, no Nebula, and no coordinator contact. It
// exists so cmd/node has a lifecycle to start and stop; M2 fills in registration
// and heartbeats, M3 assignment sync.
package agent

import (
	"context"

	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

// Agent owns the donor's control loop.
type Agent struct {
	cfg   *nodeconfig.Config
	store state.Store
}

// New constructs an Agent. The store is the donor's local state seam.
func New(cfg *nodeconfig.Config, store state.Store) *Agent {
	return &Agent{cfg: cfg, store: store}
}

// Run blocks until ctx is cancelled. M1: no work is performed.
func (a *Agent) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
