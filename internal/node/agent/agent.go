// Package agent runs the donor's control loop over the federation mTLS client:
// register once (persisted), then heartbeat on the negotiated cadence AND poll
// /fed/v1/pins/changes to converge a durable local desired-assignment set. M4
// adds the replication loop: fetch → verify → pin → persist verified state → ack.
package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/nova-archive/nova/internal/node/transfer"
)

// PubKeySink receives the coordinator's repair-token public key as it is
// delivered on each heartbeat (D-M4.1-18). Satisfied by *source.KeyProvider.
// The agent does not import the source package; this tiny interface keeps the
// donor dependency boundary clean and the wiring testable.
type PubKeySink interface {
	Set(pub ed25519.PublicKey)
}

// Client is the donor's view of the coordinator federation API. The real impl is
// agent.HTTPClient (mTLS); tests inject a fake.
type Client interface {
	Register(ctx context.Context, req wire.RegisterRequest) (wire.RegisterResponse, error)
	Heartbeat(ctx context.Context, req wire.HeartbeatRequest) (wire.HeartbeatResponse, error)
	GetChanges(ctx context.Context, sinceSeq int64) (wire.ChangesResponse, error)
	GetSnapshot(ctx context.Context, cursor string, epoch int64) (wire.SnapshotResponse, error)
	// M4: ack/fail replication outcomes.
	Ack(ctx context.Context, cid string, a wire.Ack) error
	Fail(ctx context.Context, cid string, f wire.Fail) error
}

// blockstore is the local IPFS pin store the donor replicates into.
// Satisfied by *ipfsclient.Client without importing that package here.
type blockstore interface {
	AddDeterministic(ctx context.Context, envelope []byte) (string, error)
	Has(ctx context.Context, cid string) (bool, error)
	Unpin(ctx context.Context, cid string) error
	RepoStoredBytes(ctx context.Context) (int64, error)
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

	// M4 replication fields (nil when WithSource not used).
	fetcher    transfer.SourceFetcher
	pinner     blockstore
	progress   *state.FileProgressStore
	storageMax int64

	// sources caches the most-recent *wire.ChangeSource per CID, set by syncOnce.
	sources map[string]*wire.ChangeSource

	// pubkeySink, when set, receives the coordinator repair pubkey from each
	// heartbeat (M4.1 read-source). nil when the donor is not a read source.
	pubkeySink PubKeySink

	// nodeID is this donor's own federation node id (from registration), used to
	// verify a donor↔donor repair grant is bound to THIS donor before fetching
	// (M5, D-M5-8a). Empty until Run loads/obtains it.
	nodeID string
}

// New constructs an Agent. hb/poll are the initial heartbeat + pins-poll cadences
// (each overridden by config_updates).
func New(cfg *nodeconfig.Config, reg state.RegistrationStore, cursor state.Store, asg state.AssignmentStore, client Client, hb, poll time.Duration) *Agent {
	return &Agent{
		cfg: cfg, reg: reg, cursor: cursor, assignments: asg, client: client,
		hbInterval: hb, pollInterval: poll,
		sources: map[string]*wire.ChangeSource{},
	}
}

// WithSource wires the M4 replication fields onto an existing Agent.
func WithSource(a *Agent, fetcher transfer.SourceFetcher, pinner blockstore, progress *state.FileProgressStore, storageMax int64) *Agent {
	a.fetcher = fetcher
	a.pinner = pinner
	a.progress = progress
	a.storageMax = storageMax
	return a
}

// WithPubKeySink wires the M4.1 read-source pubkey sink onto an existing Agent
// so each heartbeat's RepairTokenPublicKey is captured for the source server.
func WithPubKeySink(a *Agent, sink PubKeySink) *Agent {
	a.pubkeySink = sink
	return a
}

func (a *Agent) registerReq() wire.RegisterRequest {
	caps := []string{wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer}
	if a.cfg.SourceNebulaAddr != "" {
		// A donor that runs a source server can serve BOTH coordinator reads and
		// donor↔donor repair from the same endpoint (D-M5-8c). Advertising
		// repair-stream/v1 is what makes the coordinator eligible to pick this donor
		// as a repair SOURCE; a non-advertiser stays read-sourceable only.
		caps = append(caps, wire.CapReadSource, wire.CapRepairStream)
	}
	return wire.RegisterRequest{
		SupportedProtocols:         []string{wire.ProtocolV1},
		Capabilities:               caps,
		BandwidthBudgetBytesPerDay: a.cfg.BandwidthBudgetBytesPerDay,
		SourceNebulaAddr:           a.cfg.SourceNebulaAddr,
	}
}

func (a *Agent) heartbeatReq(freeBytes, storedBytes int64) wire.HeartbeatRequest {
	return wire.HeartbeatRequest{
		FreeBytes:        freeBytes,
		StoredBytes:      storedBytes,
		SourceNebulaAddr: a.cfg.SourceNebulaAddr,
	}
}

// captureRepairPubKey decodes the coordinator's repair pubkey from a heartbeat
// response and installs it in the sink (D-M4.1-18). It is a no-op when the donor
// is not a read source (no sink) or the field is empty (pre-M4 coordinator). A
// malformed key is logged and ignored, leaving the previous key in place.
func (a *Agent) captureRepairPubKey(wireKey string) {
	if a.pubkeySink == nil || wireKey == "" {
		return
	}
	pub, err := wire.DecodePublicKey(wireKey)
	if err != nil {
		slog.Warn("node.source.bad_repair_pubkey", "err", err)
		return
	}
	a.pubkeySink.Set(pub)
}

// Run blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if existing, ok, err := a.reg.LoadRegistration(ctx); err != nil {
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
		a.nodeID = resp.NodeID
		slog.Info("nova-node registered", "node_id", resp.NodeID, "protocol", resp.SelectedProtocol)
	} else {
		a.nodeID = existing.NodeID
	}

	// M4: reconcile any verified-ack-pending state before the loop starts.
	if a.progress != nil {
		a.ReconcilePendingAcks(ctx)
	}

	cursor, _ := a.cursor.Cursor()
	cursor = a.syncOnce(ctx, cursor) // immediate first sync: learn assignments / catch snapshot_required without waiting a full poll interval
	if a.progress != nil {
		a.ReplicatePending(ctx)
	}
	hb := time.NewTicker(a.hbInterval)
	defer hb.Stop()
	poll := time.NewTicker(a.pollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hb.C:
			resp, err := a.client.Heartbeat(ctx, a.heartbeatReq(0, 0))
			if err != nil {
				slog.Warn("nova-node heartbeat failed", "err", err)
				continue
			}
			a.captureRepairPubKey(resp.RepairTokenPublicKey)
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
			if a.progress != nil {
				a.ReplicatePending(ctx)
			}
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
	var assigns []wire.PinChange
	var unpins []wire.PinChange
	for _, ch := range resp.Changes {
		if ch.Kind != wire.ChangeKindAssign && ch.Kind != wire.ChangeKindUnpin {
			slog.Error("node.sync.failclosed", "kind", ch.Kind, "seq", ch.Sequence)
			return a.recoverSnapshot(ctx, cursor) // fail closed: stop, re-sync from snapshot
		}
		inputs = append(inputs, state.ChangeInput{
			AssignmentID: ch.AssignmentID, Generation: ch.Generation, Kind: ch.Kind, CID: ch.CID, ByteSize: ch.ByteSize,
		})
		if ch.Kind == wire.ChangeKindAssign {
			assigns = append(assigns, ch)
		} else {
			unpins = append(unpins, ch)
		}
	}
	if len(inputs) > 0 {
		if err := a.assignments.ApplyChanges(inputs); err != nil {
			slog.Warn("node.state.write_error", "err", err)
			return cursor // do not advance the cursor past unpersisted state
		}
		// M4: cache sources and process unpins after changes are persisted.
		for _, ch := range assigns {
			if ch.Source != nil {
				src := *ch.Source
				a.sources[ch.CID] = &src
			}
		}
		if a.progress != nil {
			for _, ch := range unpins {
				a.HandleUnpin(ctx, ch.CID)
			}
		}
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

// --- M4 replication methods --------------------------------------------------

// progressMatches reports whether p is a progress record for the exact
// (assignment_id, generation) of da.
func progressMatches(p state.Progress, da state.DesiredAssignment) bool {
	return p.AssignmentID == da.AssignmentID && p.Generation == da.Generation
}

// transferMax returns a sane per-blob fetch ceiling: the declared byte size,
// padded to at least 1 MiB so small/zero-declared sizes still work.
func transferMax(byteSize int64) int64 {
	const minFetch = 1 << 20 // 1 MiB floor
	if byteSize+byteSize/2 > minFetch {
		return byteSize + byteSize/2
	}
	return minFetch
}

// ReplicatePending runs the M4 fetch→verify→pin→ack loop for each pending
// desired assignment that has a Source cached. It is called after every syncOnce.
func (a *Agent) ReplicatePending(ctx context.Context) {
	das, err := a.assignments.List()
	if err != nil {
		slog.Warn("node.replicate.list_error", "err", err)
		return
	}
	for _, da := range das {
		p, hasProg := a.progress.Get(da.CID)
		if hasProg && progressMatches(p, da) {
			switch p.State {
			case state.ProgressAckDelivered:
				continue // already done for this (assignment_id, generation)
			case state.ProgressVerifiedPending:
				// Pin already present; retry the ack without re-fetching.
				a.deliverAck(ctx, da)
				continue
			}
		}
		// Clear stale-generation progress so the CID is re-fetched at the new generation.
		if hasProg && !progressMatches(p, da) {
			if err := a.progress.Clear(da.CID); err != nil {
				slog.Warn("node.replicate.clear_stale_error", "cid", da.CID, "err", err)
			}
		}
		src, ok := a.sources[da.CID]
		if !ok {
			continue // no source yet; will retry when a source arrives
		}
		a.replicateOne(ctx, da, src)
	}
}

// destBindingOK reports whether a donor↔donor repair grant is bound to THIS donor
// and the exact assignment being processed (Rev. 5 #4). Coordinator-as-source
// grants (src.NodeID == CoordinatorSourceID) skip the check — the coordinator
// binds the dest as the synthetic id and the M4 path is unchanged. A malformed
// PARTIAL binding (one Dest* field set, the other zero) is treated as a mismatch.
// The signature is NOT verified here (the source server does that); this only
// avoids fetching under a misrouted grant, so there is no ack/fail ambiguity.
func (a *Agent) destBindingOK(src *wire.ChangeSource, da state.DesiredAssignment) bool {
	// Only a real donor source (a node uuid distinct from the synthetic coordinator
	// id) carries a dest binding to check; the coordinator path (synthetic id, or an
	// empty id in legacy callers) is unchanged.
	if src.NodeID == wire.CoordinatorSourceID || src.NodeID == "" {
		return true
	}
	claims, err := wire.DecodeClaimsUnverified(src.Token)
	if err != nil {
		return false
	}
	if (claims.DestAssignmentID == "") != (claims.DestGeneration == 0) {
		return false // malformed partial binding
	}
	return claims.DestNodeID == a.nodeID &&
		claims.DestAssignmentID == da.AssignmentID &&
		claims.DestGeneration == da.Generation
}

// replicateOne runs the storage-cap check + Verify + persist + ack for one CID.
func (a *Agent) replicateOne(ctx context.Context, da state.DesiredAssignment, src *wire.ChangeSource) {
	// Donor↔donor: refuse a misrouted grant BEFORE fetching — it never started, so
	// there is no ack/fail to send (D-M5-8a / Rev. 5 #4).
	if !a.destBindingOK(src, da) {
		slog.Warn("node.replicate.dest_binding_mismatch", "cid", da.CID, "source", src.NodeID)
		return
	}
	// Storage cap check — only when a positive cap is configured (0 = uncapped).
	if a.storageMax > 0 {
		stored, err := a.pinner.RepoStoredBytes(ctx)
		if err != nil {
			slog.Warn("node.replicate.repo_stat_error", "cid", da.CID, "err", err)
			return
		}
		if stored+da.ByteSize > a.storageMax {
			slog.Warn("node.replicate.out_of_space", "cid", da.CID)
			if ferr := a.client.Fail(ctx, da.CID, wire.Fail{
				AssignmentID: da.AssignmentID, Generation: da.Generation, CID: da.CID,
				Reason: wire.FailReasonOutOfSpace,
			}); ferr != nil {
				slog.Warn("node.replicate.fail_error", "cid", da.CID, "err", ferr)
			}
			return
		}
	}

	if err := transfer.Verify(ctx, a.fetcher, a.pinner, *src, da.CID, transferMax(da.ByteSize)); err != nil {
		var fe *transfer.FailErr
		if errors.As(err, &fe) {
			slog.Warn("node.replicate.verify_failed", "cid", da.CID, "reason", fe.Reason, "err", fe.Err)
			if ferr := a.client.Fail(ctx, da.CID, wire.Fail{
				AssignmentID: da.AssignmentID, Generation: da.Generation, CID: da.CID,
				Reason: fe.Reason, Details: fe.Err.Error(),
			}); ferr != nil {
				slog.Warn("node.replicate.fail_error", "cid", da.CID, "err", ferr)
			}
		} else {
			slog.Warn("node.replicate.verify_error", "cid", da.CID, "err", err)
		}
		return
	}

	// D-M4-5: persist verified-ack-pending BEFORE acking.
	if err := a.progress.Set(da.CID, state.Progress{
		AssignmentID: da.AssignmentID, Generation: da.Generation,
		ByteSize: da.ByteSize, State: state.ProgressVerifiedPending,
	}); err != nil {
		slog.Warn("node.replicate.progress_error", "cid", da.CID, "err", err)
		return
	}
	a.deliverAck(ctx, da)
}

// deliverAck sends the ack to the coordinator and persists acked-delivered on success.
func (a *Agent) deliverAck(ctx context.Context, da state.DesiredAssignment) {
	err := a.client.Ack(ctx, da.CID, wire.Ack{
		AssignmentID:      da.AssignmentID,
		Generation:        da.Generation,
		CID:               da.CID,
		ByteSize:          da.ByteSize,
		FetchedFromNodeID: wire.CoordinatorSourceID,
	})
	if err == nil {
		if serr := a.progress.Set(da.CID, state.Progress{
			AssignmentID: da.AssignmentID, Generation: da.Generation,
			ByteSize: da.ByteSize, State: state.ProgressAckDelivered,
		}); serr != nil {
			slog.Warn("node.replicate.progress_acked_error", "cid", da.CID, "err", serr)
		}
		return
	}
	if errors.Is(err, ErrStaleAssignment) {
		// Coordinator superseded the assignment; clear progress so we re-fetch at the new generation.
		if cerr := a.progress.Clear(da.CID); cerr != nil {
			slog.Warn("node.replicate.clear_stale_error", "cid", da.CID, "err", cerr)
		}
		return
	}
	// Transient error: keep verified-pending so ReconcilePendingAcks retries.
	slog.Warn("node.replicate.ack_error", "cid", da.CID, "err", err)
}

// HandleUnpin clears progress for cid, drops the source-cache entry, and removes the local pin.
func (a *Agent) HandleUnpin(ctx context.Context, cid string) {
	if err := a.progress.Clear(cid); err != nil {
		slog.Warn("node.replicate.clear_unpin_error", "cid", cid, "err", err)
	}
	delete(a.sources, cid)
	if err := a.pinner.Unpin(ctx, cid); err != nil {
		slog.Warn("node.replicate.unpin_error", "cid", cid, "err", err)
	}
}

// ReconcilePendingAcks retries ack delivery for all verified-ack-pending entries
// on startup. If the pin has been lost, progress is cleared so re-fetch happens.
func (a *Agent) ReconcilePendingAcks(ctx context.Context) {
	das, err := a.assignments.List()
	if err != nil {
		slog.Warn("node.reconcile.list_error", "err", err)
		return
	}
	// Build a quick lookup: cid → desired assignment.
	desired := make(map[string]state.DesiredAssignment, len(das))
	for _, da := range das {
		desired[da.CID] = da
	}

	for _, entry := range a.progress.PendingAcks() {
		da, ok := desired[entry.CID]
		if !ok || !progressMatches(entry.Progress, da) {
			// Progress is stale (no matching desired assignment or generation changed).
			if cerr := a.progress.Clear(entry.CID); cerr != nil {
				slog.Warn("node.reconcile.clear_error", "cid", entry.CID, "err", cerr)
			}
			continue
		}
		// Re-check the pin is still present.
		has, err := a.pinner.Has(ctx, entry.CID)
		if err != nil {
			slog.Warn("node.reconcile.has_error", "cid", entry.CID, "err", err)
			continue
		}
		if !has {
			// Pin was lost; clear progress so the CID is re-fetched.
			if cerr := a.progress.Clear(entry.CID); cerr != nil {
				slog.Warn("node.reconcile.clear_lost_error", "cid", entry.CID, "err", cerr)
			}
			continue
		}
		a.deliverAck(ctx, da)
	}
}
