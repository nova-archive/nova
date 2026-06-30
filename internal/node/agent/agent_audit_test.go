package agent

import (
	"context"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

// TestAgentAdvertisesAuditCapability verifies that when SourceNebulaAddr is set
// in config the agent includes wire.CapAuditBlockHash in the register request.
// The audit endpoint is served by the same read-source mTLS server, so a donor
// that is not a read source cannot serve audits either.
func TestAgentAdvertisesAuditCapability(t *testing.T) {
	const addr = "10.100.0.5:9200"
	cfg := &nodeconfig.Config{
		BandwidthBudgetBytesPerDay: 1,
		SourceNebulaAddr:           addr,
	}
	cc := &captureClient{}
	a := New(cfg,
		state.NewFileRegistrationStore(t.TempDir()),
		state.NewFileStore(t.TempDir()),
		state.NewFileAssignmentStore(t.TempDir()),
		cc, 20*time.Millisecond, time.Hour,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	regReq := cc.lastRegReq.Load()
	if regReq == nil {
		t.Fatal("no register request captured")
	}
	hasAuditCap := false
	for _, c := range regReq.Capabilities {
		if c == wire.CapAuditBlockHash {
			hasAuditCap = true
			break
		}
	}
	if !hasAuditCap {
		t.Fatalf("register capabilities %v missing %q", regReq.Capabilities, wire.CapAuditBlockHash)
	}
}
