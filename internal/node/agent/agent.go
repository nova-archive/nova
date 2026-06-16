// Package agent runs the donor's register→heartbeat loop over the federation
// mTLS client. M2: register once (persisted), then heartbeat on the negotiated
// interval, honoring config_updates and backing off on transport errors. The
// pins/changes poll is M3.
package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

// Client is the donor's view of the coordinator federation API. The real impl
// is agent.HTTPClient (mTLS); tests inject a fake.
type Client interface {
	Register(ctx context.Context, req wire.RegisterRequest) (wire.RegisterResponse, error)
	Heartbeat(ctx context.Context, req wire.HeartbeatRequest) (wire.HeartbeatResponse, error)
}

// Agent owns the donor control loop.
type Agent struct {
	cfg      *nodeconfig.Config
	store    state.RegistrationStore
	client   Client
	interval time.Duration
}

// New constructs an Agent. interval is the initial heartbeat cadence (overridden
// by config_updates).
func New(cfg *nodeconfig.Config, store state.RegistrationStore, client Client, interval time.Duration) *Agent {
	return &Agent{cfg: cfg, store: store, client: client, interval: interval}
}

func (a *Agent) registerReq() wire.RegisterRequest {
	return wire.RegisterRequest{
		SupportedProtocols:         []string{wire.ProtocolV1},
		Capabilities:               []string{}, // M2: advertise nothing beyond base protocol
		BandwidthBudgetBytesPerDay: a.cfg.BandwidthBudgetBytesPerDay,
	}
}

// Run blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if _, ok, err := a.store.LoadRegistration(ctx); err != nil {
		return err
	} else if !ok {
		resp, err := a.client.Register(ctx, a.registerReq())
		if err != nil {
			return err
		}
		if err := a.store.SaveRegistration(ctx, state.Registration{
			NodeID:           resp.NodeID,
			SelectedProtocol: resp.SelectedProtocol,
			RegisteredAt:     time.Now().UTC(),
		}); err != nil {
			return err
		}
		slog.Info("nova-node registered", "node_id", resp.NodeID, "protocol", resp.SelectedProtocol)
	}

	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			resp, err := a.client.Heartbeat(ctx, wire.HeartbeatRequest{})
			if err != nil {
				slog.Warn("nova-node heartbeat failed", "err", err)
				continue
			}
			// Apply a coordinator-requested cadence change immediately, and only
			// when it actually differs (no redundant per-tick Reset).
			if resp.ConfigUpdates != nil && resp.ConfigUpdates.HeartbeatIntervalSeconds > 0 {
				next := time.Duration(resp.ConfigUpdates.HeartbeatIntervalSeconds) * time.Second
				if next != a.interval {
					a.interval = next
					ticker.Reset(next)
				}
			}
		}
	}
}
