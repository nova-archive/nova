// Package agent runs the donor's control loop over the federation mTLS client:
// register once (persisted), then heartbeat on the negotiated cadence AND poll
// /fed/v1/pins/changes to converge a durable local desired-assignment set. M3 is
// sync-only — the donor learns assignments and recovers via snapshot, but it does
// NOT fetch bytes and NEVER acks (an ack means verified local storage — that is M4).
package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

// Client is the donor's view of the coordinator federation API. The real impl is
// agent.HTTPClient (mTLS); tests inject a fake. NOTE: there is deliberately NO
// Ack/Fail here — the donor does not ack in M3 (that is M4's fetch→verify→ack).
type Client interface {
	Register(ctx context.Context, req wire.RegisterRequest) (wire.RegisterResponse, error)
	Heartbeat(ctx context.Context, req wire.HeartbeatRequest) (wire.HeartbeatResponse, error)
	GetChanges(ctx context.Context, sinceSeq int64) (wire.ChangesResponse, error)
	GetSnapshot(ctx context.Context, cursor string, epoch int64) (wire.SnapshotResponse, error)
}

// Agent owns the donor control loop.
type Agent struct {
	cfg          *nodeconfig.Config
	reg          state.RegistrationStore
	cursor       state.Store
	assignments  state.AssignmentStore
	client       Client
	hbInterval   time.Duration
	pollInterval time.Duration
}

// New constructs an Agent. hb/poll are the initial heartbeat + pins-poll cadences
// (each overridden by config_updates).
func New(cfg *nodeconfig.Config, reg state.RegistrationStore, cursor state.Store, asg state.AssignmentStore, client Client, hb, poll time.Duration) *Agent {
	return &Agent{cfg: cfg, reg: reg, cursor: cursor, assignments: asg, client: client, hbInterval: hb, pollInterval: poll}
}

func (a *Agent) registerReq() wire.RegisterRequest {
	return wire.RegisterRequest{
		SupportedProtocols:         []string{wire.ProtocolV1},
		Capabilities:               []string{wire.CapPinChangeLog, wire.CapSnapshot},
		BandwidthBudgetBytesPerDay: a.cfg.BandwidthBudgetBytesPerDay,
	}
}

// Run blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if _, ok, err := a.reg.LoadRegistration(ctx); err != nil {
		return err
	} else if !ok {
		resp, err := a.client.Register(ctx, a.registerReq())
		if err != nil {
			return err
		}
		if err := a.reg.SaveRegistration(ctx, state.Registration{
			NodeID:           resp.NodeID,
			SelectedProtocol: resp.SelectedProtocol,
			RegisteredAt:     time.Now().UTC(),
		}); err != nil {
			return err
		}
		slog.Info("nova-node registered", "node_id", resp.NodeID, "protocol", resp.SelectedProtocol)
	}

	cursor, _ := a.cursor.Cursor()
	cursor = a.syncOnce(ctx, cursor) // immediate first sync: learn assignments / catch snapshot_required without waiting a full poll interval
	hb := time.NewTicker(a.hbInterval)
	defer hb.Stop()
	poll := time.NewTicker(a.pollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hb.C:
			resp, err := a.client.Heartbeat(ctx, wire.HeartbeatRequest{})
			if err != nil {
				slog.Warn("nova-node heartbeat failed", "err", err)
				continue
			}
			if u := resp.ConfigUpdates; u != nil {
				if u.HeartbeatIntervalSeconds > 0 {
					if d := time.Duration(u.HeartbeatIntervalSeconds) * time.Second; d != a.hbInterval {
						a.hbInterval = d
						hb.Reset(d)
					}
				}
				if u.PinsPollIntervalSeconds > 0 {
					if d := time.Duration(u.PinsPollIntervalSeconds) * time.Second; d != a.pollInterval {
						a.pollInterval = d
						poll.Reset(d)
					}
				}
			}
		case <-poll.C:
			cursor = a.syncOnce(ctx, cursor)
		}
	}
}

// syncOnce pulls one batch of changes (or recovers via snapshot) and returns the
// new cursor. It applies changes idempotently and NEVER acks (M4 owns ack).
func (a *Agent) syncOnce(ctx context.Context, cursor int64) int64 {
	resp, err := a.client.GetChanges(ctx, cursor)
	if errors.Is(err, ErrSnapshotRequired) {
		return a.recoverSnapshot(ctx, cursor)
	}
	if err != nil {
		slog.Warn("node.sync.poll_failed", "err", err)
		return cursor
	}
	inputs := make([]state.ChangeInput, 0, len(resp.Changes))
	for _, ch := range resp.Changes {
		if ch.Kind != wire.ChangeKindAssign && ch.Kind != wire.ChangeKindUnpin {
			slog.Error("node.sync.failclosed", "kind", ch.Kind, "seq", ch.Sequence)
			return a.recoverSnapshot(ctx, cursor) // fail closed: stop, re-sync from snapshot
		}
		inputs = append(inputs, state.ChangeInput{
			AssignmentID: ch.AssignmentID, Generation: ch.Generation, Kind: ch.Kind, CID: ch.CID, ByteSize: ch.ByteSize,
		})
	}
	if err := a.assignments.ApplyChanges(inputs); err != nil {
		slog.Warn("node.state.write_error", "err", err)
		return cursor // do not advance the cursor past unpersisted state
	}
	if err := a.cursor.SetCursor(resp.NextSeq); err != nil {
		slog.Warn("node.state.write_error", "err", err)
		return cursor
	}
	slog.Info("node.sync.applied", "from_seq", cursor, "to_seq", resp.NextSeq, "applied", len(inputs))
	return resp.NextSeq
}

// recoverSnapshot rebuilds the desired set from a full snapshot and returns the
// new cursor (= snapshot_epoch) ONLY after both the set and the cursor persist;
// on any error it returns oldCursor unchanged, so neither in-memory nor durable
// cursor state ever skips unpersisted assignments ("set first, cursor second").
func (a *Agent) recoverSnapshot(ctx context.Context, oldCursor int64) int64 {
	var all []state.DesiredAssignment
	cursor := ""
	var epoch int64
	for {
		resp, err := a.client.GetSnapshot(ctx, cursor, epoch)
		if errors.Is(err, ErrSnapshotEpochChanged) {
			all, cursor, epoch = nil, "", 0 // restart from page 1
			continue
		}
		if err != nil {
			slog.Warn("node.sync.snapshot_failed", "err", err)
			return oldCursor
		}
		epoch = resp.SnapshotEpoch
		for _, it := range resp.Data {
			all = append(all, state.DesiredAssignment{CID: it.CID, AssignmentID: it.AssignmentID, Generation: it.Generation, ByteSize: it.ByteSize, State: "pending"})
		}
		if resp.Cursor == "" {
			break
		}
		cursor = resp.Cursor
	}
	if err := a.assignments.Replace(all); err != nil {
		slog.Warn("node.state.write_error", "err", err)
		return oldCursor
	}
	if err := a.cursor.SetCursor(epoch); err != nil {
		slog.Warn("node.state.write_error", "err", err)
		return oldCursor
	}
	slog.Info("node.sync.recovered", "snapshot_epoch", epoch, "items", len(all))
	return epoch
}
