package agent

import (
	"context"
	"testing"

	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/nova-archive/nova/internal/node/transfer"
	"github.com/stretchr/testify/require"
)

// tokenFor builds a wire token whose claims segment carries c. The signature is a
// throwaway: DecodeClaimsUnverified (the donor's pre-fetch binding check) does not
// verify it — the source server does the real verification when the token is used.
func tokenFor(t *testing.T, c wire.Claims) string {
	t.Helper()
	si, err := wire.SigningInput(c)
	require.NoError(t, err)
	return wire.AssembleToken(si, []byte("unverified-sig"))
}

func baseRepairClaims() wire.Claims {
	return wire.Claims{
		JTI: "j", AssignmentID: "src-asg", Generation: 1, CID: "x",
		SourceNodeID: "donor-src-9", DestNodeID: "dest-node-1",
		NotBefore: 1, NotAfter: 2, MaxBytes: 1 << 20, ProtocolVersion: wire.ProtocolV1,
		DestAssignmentID: "a-dest", DestGeneration: 2,
	}
}

func TestDestVerifiesDestBindingBeforeFetch(t *testing.T) {
	const cid = "bafyrepair1"
	h := newAgentHarness(t, cid)
	h.agent.nodeID = "dest-node-1"
	called := 0
	h.agent.fetcher = &countingFetcher{onCall: func() { called++ }}
	require.NoError(t, h.asgStore.ApplyChanges([]state.ChangeInput{
		{CID: cid, AssignmentID: "a-dest", Generation: 2, Kind: wire.ChangeKindAssign, ByteSize: 5},
	}))
	c := baseRepairClaims()
	c.CID = cid
	c.DestAssignmentID = "WRONG-ASG" // grant bound to a different assignment
	h.agent.sources[cid] = &wire.ChangeSource{NodeID: "donor-src-9", Token: tokenFor(t, c)}

	h.agent.ReplicatePending(context.Background())

	require.Equal(t, 0, called, "must not fetch under a mismatched dest binding")
	require.False(t, h.client.ackedCID(cid), "no ack")
	require.Empty(t, h.client.lastFailReason(cid), "no fail — the transfer never started")
}

func TestMalformedPartialDestFieldsRefused(t *testing.T) {
	const cid = "bafyrepair2"
	h := newAgentHarness(t, cid)
	h.agent.nodeID = "dest-node-1"
	called := 0
	h.agent.fetcher = &countingFetcher{onCall: func() { called++ }}
	require.NoError(t, h.asgStore.ApplyChanges([]state.ChangeInput{
		{CID: cid, AssignmentID: "a-dest", Generation: 2, Kind: wire.ChangeKindAssign, ByteSize: 5},
	}))
	c := baseRepairClaims()
	c.CID = cid
	c.DestAssignmentID = "a-dest"
	c.DestGeneration = 0 // partial: assignment set, generation zero
	h.agent.sources[cid] = &wire.ChangeSource{NodeID: "donor-src-9", Token: tokenFor(t, c)}

	h.agent.ReplicatePending(context.Background())

	require.Equal(t, 0, called, "malformed partial binding must refuse before fetch")
	require.False(t, h.client.ackedCID(cid))
}

func TestDestBindingMatchProceedsToAck(t *testing.T) {
	const cid = "bafyrepair3"
	h := newAgentHarness(t, cid) // default fetcher body "hello"; pinner re-imports to cid
	h.agent.nodeID = "dest-node-1"
	require.NoError(t, h.asgStore.ApplyChanges([]state.ChangeInput{
		{CID: cid, AssignmentID: "a-dest", Generation: 2, Kind: wire.ChangeKindAssign, ByteSize: 5},
	}))
	c := baseRepairClaims()
	c.CID = cid
	c.DestAssignmentID = "a-dest"
	c.DestGeneration = 2
	h.agent.sources[cid] = &wire.ChangeSource{NodeID: "donor-src-9", Token: tokenFor(t, c)}

	h.agent.ReplicatePending(context.Background())

	require.True(t, h.client.ackedCID(cid), "a matching dest binding proceeds to fetch+verify+ack")
}

func TestSourceShortReadDestNoAck(t *testing.T) {
	// A truncated/corrupt source body re-imports to a DIFFERENT root CID, so Verify
	// returns a cid_mismatch FailErr — the destination fails, never acks (Rev. 5 #10).
	ctx := context.Background()
	fetcher := &fakeFetcher{body: "truncated-body"}
	pinner := newFakePinner("bafyDIFFERENT")
	err := transfer.Verify(ctx, fetcher, pinner,
		wire.ChangeSource{NodeID: "donor-src-9", Token: "t"}, "bafyEXPECTED", 1<<20)
	var fe *transfer.FailErr
	require.ErrorAs(t, err, &fe)
	require.Equal(t, wire.FailReasonCIDMismatch, fe.Reason)
}
