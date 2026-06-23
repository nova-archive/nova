package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/nova-archive/nova/internal/federation/wire"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/nova-archive/nova/internal/node/transfer"
)

// ---- fake Client for replication tests -------------------------------------

type repClient struct {
	mu         sync.Mutex
	ackedCIDs  map[string]wire.Ack
	failedCIDs map[string]wire.Fail
	ackErr     error // if set, Ack returns this error
}

func newRepClient() *repClient {
	return &repClient{
		ackedCIDs:  map[string]wire.Ack{},
		failedCIDs: map[string]wire.Fail{},
	}
}

func (c *repClient) Register(context.Context, wire.RegisterRequest) (wire.RegisterResponse, error) {
	return wire.RegisterResponse{NodeID: "n1", SelectedProtocol: wire.ProtocolV1}, nil
}
func (c *repClient) Heartbeat(context.Context, wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	return wire.HeartbeatResponse{}, nil
}
func (c *repClient) GetChanges(context.Context, int64) (wire.ChangesResponse, error) {
	return wire.ChangesResponse{}, nil
}
func (c *repClient) GetSnapshot(context.Context, string, int64) (wire.SnapshotResponse, error) {
	return wire.SnapshotResponse{}, nil
}
func (c *repClient) Ack(_ context.Context, cid string, a wire.Ack) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ackErr != nil {
		return c.ackErr
	}
	c.ackedCIDs[cid] = a
	return nil
}
func (c *repClient) Fail(_ context.Context, cid string, f wire.Fail) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failedCIDs[cid] = f
	return nil
}

// helpers

func (c *repClient) ackedCID(cid string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.ackedCIDs[cid]
	return ok
}
func (c *repClient) lastAckGeneration(cid string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ackedCIDs[cid].Generation
}
func (c *repClient) lastFailReason(cid string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failedCIDs[cid].Reason
}

// ---- fake fetcher ----------------------------------------------------------

type fakeFetcher struct {
	body string
	err  error
}

func (f *fakeFetcher) Fetch(_ context.Context, _ wire.ChangeSource, _ string, _ int64) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

// ---- fake pinner -----------------------------------------------------------

type fakePinner struct {
	mu      sync.Mutex
	has     map[string]bool
	pinRoot string // returned by AddDeterministic (should match CID under test)
	addErr  error
}

func newFakePinner(root string) *fakePinner {
	return &fakePinner{
		has:     map[string]bool{},
		pinRoot: root,
	}
}

func (p *fakePinner) AddDeterministic(_ context.Context, _ []byte) (string, error) {
	if p.addErr != nil {
		return "", p.addErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.has[p.pinRoot] = true
	return p.pinRoot, nil
}
func (p *fakePinner) Has(_ context.Context, cid string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.has[cid], nil
}
func (p *fakePinner) Unpin(_ context.Context, cid string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.has, cid)
	return nil
}
func (p *fakePinner) RepoStoredBytes(_ context.Context) (int64, error) {
	return 0, nil // always fits
}

// ---- harness ---------------------------------------------------------------

type testHarness struct {
	agent    *Agent
	client   *repClient
	pinner   *fakePinner
	progress *state.FileProgressStore
	asgStore *state.FileAssignmentStore
}

func newAgentHarness(t *testing.T, cid string) *testHarness {
	t.Helper()
	dir := t.TempDir()
	asgStore := state.NewFileAssignmentStore(dir)
	progress, err := state.NewFileProgressStore(dir)
	if err != nil {
		t.Fatalf("NewFileProgressStore: %v", err)
	}
	client := newRepClient()
	pinner := newFakePinner(cid)

	a := New(
		&nodeconfig.Config{},
		state.NewFileRegistrationStore(dir),
		state.NewFileStore(dir),
		asgStore,
		client,
		0, 0,
	)
	WithSource(a, &fakeFetcher{body: "hello"}, pinner, progress, 1<<40 /* 1TiB cap */)

	return &testHarness{
		agent:    a,
		client:   client,
		pinner:   pinner,
		progress: progress,
		asgStore: asgStore,
	}
}

// seedAssignment adds a desired assignment AND caches the source so ReplicatePending fires.
func (h *testHarness) seedAssignment(t *testing.T, cid, assignID string, gen int64, byteSize int64) {
	t.Helper()
	if err := h.asgStore.ApplyChanges([]state.ChangeInput{
		{CID: cid, AssignmentID: assignID, Generation: gen, Kind: wire.ChangeKindAssign, ByteSize: byteSize},
	}); err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}
	src := &wire.ChangeSource{NodeID: wire.CoordinatorSourceID, Token: "tok"}
	h.agent.sources[cid] = src
}

// ---- tests -----------------------------------------------------------------

func TestReplicateOneHappyPathAcks(t *testing.T) {
	const cid = "bafytest1"
	h := newAgentHarness(t, cid)
	h.seedAssignment(t, cid, "a1", 1, 5)

	h.agent.ReplicatePending(context.Background())

	if !h.client.ackedCID(cid) {
		t.Fatal("expected ack for cid, got none")
	}
	if gen := h.client.lastAckGeneration(cid); gen != 1 {
		t.Fatalf("ack generation = %d, want 1", gen)
	}
	// Progress should be acked-delivered.
	p, ok := h.progress.Get(cid)
	if !ok || p.State != state.ProgressAckDelivered {
		t.Fatalf("progress state = %q (ok=%v), want acked-delivered", p.State, ok)
	}
}

func TestReplicateOneCIDMismatchFails(t *testing.T) {
	const assignedCID = "bafyassigned"
	// pinner returns a different CID → triggers FailReasonCIDMismatch
	dir := t.TempDir()
	asgStore := state.NewFileAssignmentStore(dir)
	progress, _ := state.NewFileProgressStore(dir)
	client := newRepClient()
	// Pinner returns "bafyOTHER" but assignment is for "bafyassigned"
	pinner := newFakePinner("bafyOTHER")

	a := New(&nodeconfig.Config{}, state.NewFileRegistrationStore(dir), state.NewFileStore(dir), asgStore, client, 0, 0)
	// Use a fetcher that returns some bytes; pinner will return wrong CID
	WithSource(a, &fakeFetcher{body: "x"}, pinner, progress, 1<<40)

	if err := asgStore.ApplyChanges([]state.ChangeInput{
		{CID: assignedCID, AssignmentID: "a1", Generation: 1, Kind: wire.ChangeKindAssign, ByteSize: 1},
	}); err != nil {
		t.Fatal(err)
	}
	a.sources[assignedCID] = &wire.ChangeSource{Token: "tok"}

	a.ReplicatePending(context.Background())

	reason := client.lastFailReason(assignedCID)
	if reason != wire.FailReasonCIDMismatch {
		t.Fatalf("fail reason = %q, want cid_mismatch", reason)
	}
	if client.ackedCID(assignedCID) {
		t.Fatal("must not ack on cid_mismatch")
	}
}

func TestReassignAtNewGenerationIsNotSkipped(t *testing.T) {
	const cid = "bafyreassign"
	h := newAgentHarness(t, cid)
	h.seedAssignment(t, cid, "a1", 1, 5)

	// First replication: ack generation 1.
	h.agent.ReplicatePending(context.Background())
	if !h.client.ackedCID(cid) {
		t.Fatal("gen-1 ack expected")
	}

	// Now reassign at generation 2.
	if err := h.asgStore.ApplyChanges([]state.ChangeInput{
		{CID: cid, AssignmentID: "a1", Generation: 2, Kind: wire.ChangeKindAssign, ByteSize: 5},
	}); err != nil {
		t.Fatal(err)
	}
	h.agent.sources[cid] = &wire.ChangeSource{Token: "tok2"}

	// Reset pinner so AddDeterministic is called again and re-pins.
	h.pinner.mu.Lock()
	h.pinner.has = map[string]bool{}
	h.pinner.mu.Unlock()
	// Reset client ack tracking so we detect the new ack.
	h.client.mu.Lock()
	h.client.ackedCIDs = map[string]wire.Ack{}
	h.client.mu.Unlock()

	h.agent.ReplicatePending(context.Background())

	if !h.client.ackedCID(cid) {
		t.Fatal("gen-2 ack expected but missing")
	}
	if gen := h.client.lastAckGeneration(cid); gen != 2 {
		t.Fatalf("ack generation = %d, want 2", gen)
	}
}

func TestUnpinClearsProgressAndUnpinsLocally(t *testing.T) {
	const cid = "bafyunpin"
	h := newAgentHarness(t, cid)
	h.seedAssignment(t, cid, "a1", 1, 5)

	// Replicate so there is an acked-delivered progress entry and a local pin.
	h.agent.ReplicatePending(context.Background())
	if !h.client.ackedCID(cid) {
		t.Fatal("pre-condition: ack not sent")
	}

	// Simulate coordinator sending an unpin.
	h.agent.HandleUnpin(context.Background(), cid)

	// Progress should be cleared.
	if _, ok := h.progress.Get(cid); ok {
		t.Fatal("progress should be cleared after unpin")
	}
	// Local pin should be removed.
	if has, _ := h.pinner.Has(context.Background(), cid); has {
		t.Fatal("pin should be removed after HandleUnpin")
	}
}

func TestStartupReconcileRetriesAckWhenStillPinnedAndMatching(t *testing.T) {
	const cid = "bafyreconcile"
	h := newAgentHarness(t, cid)
	h.seedAssignment(t, cid, "a1", 1, 5)

	// Manually set verified-ack-pending (simulates crash after verify, before ack).
	if err := h.progress.Set(cid, state.Progress{
		AssignmentID: "a1", Generation: 1, ByteSize: 5, State: state.ProgressVerifiedPending,
	}); err != nil {
		t.Fatal(err)
	}
	// Mark the pin as present in the fake pinner.
	h.pinner.mu.Lock()
	h.pinner.has[cid] = true
	h.pinner.mu.Unlock()

	h.agent.ReconcilePendingAcks(context.Background())

	if !h.client.ackedCID(cid) {
		t.Fatal("reconcile should have retried the ack")
	}
	p, ok := h.progress.Get(cid)
	if !ok || p.State != state.ProgressAckDelivered {
		t.Fatalf("progress = %v (ok=%v), want acked-delivered", p, ok)
	}
}

func TestStartupReconcileDropsStaleGenerationProgress(t *testing.T) {
	const cid = "bafystalegen"
	h := newAgentHarness(t, cid)
	// Desired assignment is gen 2.
	h.seedAssignment(t, cid, "a1", 2, 5)

	// Progress says gen 1 was verified-ack-pending (stale).
	if err := h.progress.Set(cid, state.Progress{
		AssignmentID: "a1", Generation: 1, ByteSize: 5, State: state.ProgressVerifiedPending,
	}); err != nil {
		t.Fatal(err)
	}

	h.agent.ReconcilePendingAcks(context.Background())

	// No ack should be sent.
	if h.client.ackedCID(cid) {
		t.Fatal("stale-gen reconcile must not ack")
	}
	// Progress should be cleared.
	if _, ok := h.progress.Get(cid); ok {
		t.Fatal("stale progress should be cleared")
	}
}

func TestStartupReconcileReFetchesWhenPinLost(t *testing.T) {
	const cid = "bafypinlost"
	h := newAgentHarness(t, cid)
	h.seedAssignment(t, cid, "a1", 1, 5)

	// verified-ack-pending but pin is NOT in the pinner (lost).
	if err := h.progress.Set(cid, state.Progress{
		AssignmentID: "a1", Generation: 1, ByteSize: 5, State: state.ProgressVerifiedPending,
	}); err != nil {
		t.Fatal(err)
	}
	// Has returns false (pin not present).

	h.agent.ReconcilePendingAcks(context.Background())

	// No ack — pin is lost.
	if h.client.ackedCID(cid) {
		t.Fatal("must not ack when pin is lost")
	}
	// Progress cleared so re-fetch will happen on next ReplicatePending.
	if _, ok := h.progress.Get(cid); ok {
		t.Fatal("progress should be cleared when pin is lost")
	}
}

// TestReplicateFetchError exercises the network_error path when the fetcher fails.
func TestReplicateFetchError(t *testing.T) {
	const cid = "bafyfetcherr"
	dir := t.TempDir()
	asgStore := state.NewFileAssignmentStore(dir)
	progress, _ := state.NewFileProgressStore(dir)
	client := newRepClient()
	pinner := newFakePinner(cid)

	a := New(&nodeconfig.Config{}, state.NewFileRegistrationStore(dir), state.NewFileStore(dir), asgStore, client, 0, 0)
	fetcher := &fakeFetcher{err: fmt.Errorf("connection refused")}
	WithSource(a, fetcher, pinner, progress, 1<<40)

	if err := asgStore.ApplyChanges([]state.ChangeInput{
		{CID: cid, AssignmentID: "a1", Generation: 1, Kind: wire.ChangeKindAssign, ByteSize: 5},
	}); err != nil {
		t.Fatal(err)
	}
	a.sources[cid] = &wire.ChangeSource{Token: "tok"}

	a.ReplicatePending(context.Background())

	if client.ackedCID(cid) {
		t.Fatal("must not ack on fetch error")
	}
	if r := client.lastFailReason(cid); r != wire.FailReasonNetworkError {
		t.Fatalf("fail reason = %q, want network_error", r)
	}
}

// TestReplicateSourceMissingFails exercises the blob_unavailable path.
func TestReplicateSourceMissingFails(t *testing.T) {
	const cid = "bafymissing"
	dir := t.TempDir()
	asgStore := state.NewFileAssignmentStore(dir)
	progress, _ := state.NewFileProgressStore(dir)
	client := newRepClient()
	pinner := newFakePinner(cid)

	a := New(&nodeconfig.Config{}, state.NewFileRegistrationStore(dir), state.NewFileStore(dir), asgStore, client, 0, 0)
	fetcher := &fakeFetcher{err: transfer.ErrSourceMissing}
	WithSource(a, fetcher, pinner, progress, 1<<40)

	if err := asgStore.ApplyChanges([]state.ChangeInput{
		{CID: cid, AssignmentID: "a1", Generation: 1, Kind: wire.ChangeKindAssign, ByteSize: 5},
	}); err != nil {
		t.Fatal(err)
	}
	a.sources[cid] = &wire.ChangeSource{Token: "tok"}

	a.ReplicatePending(context.Background())

	if client.ackedCID(cid) {
		t.Fatal("must not ack on source_missing")
	}
	if r := client.lastFailReason(cid); r != wire.FailReasonBlobUnavailable {
		t.Fatalf("fail reason = %q, want blob_unavailable", r)
	}
}

// TestAckErrorKeepsVerifiedPending verifies that a transient ack error leaves
// the progress in verified-ack-pending so reconcile can retry.
func TestAckErrorKeepsVerifiedPending(t *testing.T) {
	const cid = "bafyackerr"
	h := newAgentHarness(t, cid)
	h.seedAssignment(t, cid, "a1", 1, 5)
	h.client.ackErr = errors.New("network timeout")

	h.agent.ReplicatePending(context.Background())

	// No delivered ack.
	if h.client.ackedCID(cid) {
		t.Fatal("must not record ack on error")
	}
	// But progress should be verified-ack-pending.
	p, ok := h.progress.Get(cid)
	if !ok || p.State != state.ProgressVerifiedPending {
		t.Fatalf("progress = %v (ok=%v), want verified-ack-pending", p, ok)
	}
}

// TestStaleAckClearsProgress verifies ErrStaleAssignment clears progress.
func TestStaleAckClearsProgress(t *testing.T) {
	const cid = "bafystale"
	h := newAgentHarness(t, cid)
	h.seedAssignment(t, cid, "a1", 1, 5)
	h.client.ackErr = ErrStaleAssignment

	h.agent.ReplicatePending(context.Background())

	// Progress cleared on stale.
	if _, ok := h.progress.Get(cid); ok {
		t.Fatal("progress should be cleared on stale_assignment")
	}
}
