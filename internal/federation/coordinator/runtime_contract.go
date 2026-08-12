package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/release"
)

// The runtime-contract state machine (P2-M7.3, D-M7.3-7c).
//
// effective_capabilities moves through exactly these transitions. Anything not
// listed is a defect.
//
//	FROM                     EVENT                              TO
//	----                     -----                              --
//	—                        registration                       registration snapshot; marker NULL
//	snapshot, marker NULL    heartbeat WITH a supported contract  contract's set; marker stamped
//	snapshot, marker NULL    heartbeat WITHOUT a contract         unchanged (legacy donor)
//	runtime, marker set      heartbeat WITH a supported contract  contract's set; marker refreshed
//	runtime, marker set      heartbeat WITHOUT a contract         contract unknown; optional-role
//	                                                              capabilities dropped, replicas kept
//	any                      re-registration                     fresh snapshot; marker reset to NULL
//	any                      unknown FUTURE contract version      current set kept for existing work;
//	                                                              no new optional-role work granted
//
// The re-registration reset is load-bearing: without it a legacy donor that
// re-registers after eviction inherits a stamped marker, and its next
// contract-less heartbeat is misread as a rollback.

// Reported-field limits. Untrusted is not the same as unvalidated: a donor may
// claim anything, but it may not make the census unreadable or the row large.
const (
	maxReportedFieldLen = 256
	maxReportedListLen  = 32
)

// validateRuntimeContract checks shape, not truthfulness. A claim that fails
// here is malformed rather than false, and the heartbeat is rejected.
func validateRuntimeContract(c *wire.RuntimeContract) error {
	if c.Version <= 0 {
		return fmt.Errorf("runtime_contract.version %d is not positive", c.Version)
	}
	for name, v := range map[string]string{
		"client_version":     c.ClientVersion,
		"image_digest":       c.ImageDigest,
		"bundle_lock_digest": c.BundleLockDigest,
	} {
		if len(v) > maxReportedFieldLen {
			return fmt.Errorf("runtime_contract.%s is %d bytes, over the %d limit", name, len(v), maxReportedFieldLen)
		}
		if strings.ContainsAny(v, "\x00\n\r") {
			return fmt.Errorf("runtime_contract.%s contains a control character", name)
		}
	}
	for name, list := range map[string][]string{
		"capabilities":        c.Capabilities,
		"supported_protocols": c.Protocols,
	} {
		if len(list) > maxReportedListLen {
			return fmt.Errorf("runtime_contract.%s has %d entries, over the %d limit", name, len(list), maxReportedListLen)
		}
		for _, v := range list {
			if v == "" || len(v) > maxReportedFieldLen || strings.ContainsAny(v, "\x00\n\r") {
				return fmt.Errorf("runtime_contract.%s contains an unusable entry", name)
			}
		}
	}
	return nil
}

// coreProfile is the capability set a donor keeps when its runtime contract
// disappears. Everything outside it is optional-role eligibility, which a
// downgraded donor may no longer implement.
//
// It comes from the compiled-in catalog, which is what makes this evaluable
// with no network (T1.22). The production required profile is the fallback for
// a build with no generated catalog.
func coreProfile() []string {
	if c := release.Compiled(); c.Stamped() && len(c.Profiles.Core) > 0 {
		return c.Profiles.Core
	}
	return ProductionRequiredCapabilities
}

// retainedOnDowngrade narrows a capability set to the core profile.
func retainedOnDowngrade(current []string) []string {
	core := coreProfile()
	out := make([]string, 0, len(current))
	for _, c := range current {
		if slices.Contains(core, c) {
			out = append(out, c)
		}
	}
	return out
}

// applyRuntimeContract executes one transition of the state machine for a
// heartbeat. It is called AFTER the liveness update, so a rejected contract
// never costs a donor its liveness.
func (s *Server) applyRuntimeContract(ctx context.Context, nodeID uuid.UUID, c *wire.RuntimeContract) error {
	pgID := pgUUIDFrom(nodeID)

	if c != nil && c.Version > wire.RuntimeContractVersion {
		// Unknown FUTURE version. Keep the heartbeat compatible, interpret
		// nothing, and grant no new optional-role work. Conservative in the
		// direction that loses work rather than the direction that loses data.
		slog.Warn("fed.heartbeat.contract_version_unsupported",
			"node_id", nodeID, "reported_version", c.Version, "supported_version", wire.RuntimeContractVersion)
		return s.q.MarkRuntimeContractUnparseable(ctx, pgID)
	}

	if c != nil {
		caps := c.Capabilities
		if caps == nil {
			caps = []string{}
		}
		protos := c.Protocols
		if protos == nil {
			protos = []string{}
		}
		return s.q.ApplyRuntimeContract(ctx, gen.ApplyRuntimeContractParams{
			ID:               pgID,
			ClientVersion:    c.ClientVersion,
			ImageDigest:      c.ImageDigest,
			BundleLockDigest: c.BundleLockDigest,
			Capabilities:     caps,
			Protocols:        protos,
		})
	}

	// No contract. Which silence is this?
	state, err := s.q.GetRuntimeContractState(ctx, pgID)
	if err != nil {
		return err
	}
	if !state.RuntimeContractObservedAt.Valid {
		// Never observed: a pre-M7.3 donor. Its registration snapshot stands,
		// and freshness is classified as registration-only.
		return nil
	}

	// Observed before, absent now: a downgrade. Drop optional-role eligibility
	// and keep the replicas.
	retained := retainedOnDowngrade(state.EffectiveCapabilities)
	slog.Info("fed.heartbeat.contract_withdrawn",
		"node_id", nodeID, "was", state.EffectiveCapabilities, "retained", retained)
	return s.q.ClearRuntimeContractOnDowngrade(ctx, gen.ClearRuntimeContractOnDowngradeParams{
		ID: pgID, Retained: retained,
	})
}
