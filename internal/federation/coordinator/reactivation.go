package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/nova-archive/nova/internal/db/gen"
)

// Safe reactivation of an evicted donor (P2-M7.3, Task 26 step 3).
//
// # The trap this closes
//
// A donor that goes offline past the eviction threshold becomes USELESS
// INDEFINITELY, and cannot be taught otherwise:
//
//   - the agent loads its durable registration once at boot and never
//     re-registers;
//   - the coordinator rejects an evicted node's heartbeat with
//     `registration_required`;
//   - the agent reduces that response to an ordinary warning and acts on none
//     of it.
//
// So the donor heartbeats forever into a refusal it does not understand, its
// replicas sit on a machine the coordinator considers gone, and the volunteer
// sees a healthy-looking container. The fix cannot live in the client: that
// binary is already deployed on other people's machines, and telling a
// volunteer to delete state and re-enroll is precisely the re-enrollment this
// whole track exists to prevent.
//
// It therefore lives here.
//
// # Why this is safe, and what it deliberately does not do
//
// The donor is authenticated by mTLS before any of this runs, and its
// certificate fingerprint must still match the one stored at registration.
// Eviction is a LIVENESS judgement — "we have not heard from you in 30 days" —
// not a trust judgement. A node whose trust was withdrawn is `revoked`, and
// revoked stays refused.
//
// Reactivation restores PARTICIPATION, not standing:
//
//   - the node returns to `active` so its heartbeats are accepted and it can be
//     given work again;
//   - its assignment-sync state is forced to `stale`, so the next sync is a
//     full SNAPSHOT rather than a diff against a change log that has since been
//     pruned;
//   - it is NOT credited with anything it claims to hold. Its replicas were
//     retired from the desired set when it was evicted, and they come back only
//     by being re-assigned and re-acknowledged — or by a possession audit
//     answering for them. A reactivation that restored durability counting on
//     the donor's say-so would let a month-old machine assert coverage for
//     blobs it may have deleted.
//
// # It is recorded
//
// Reactivating a node the operator's own liveness policy removed is a
// privileged state change, so it writes an audit_log entry (T1.24) and an
// upgrade-style event trail is not enough. An operator who finds an evicted
// donor active again must be able to find out why.

// ErrReactivationRefused is returned when the node may not come back.
var ErrReactivationRefused = errors.New("coordinator: reactivation refused")

// AllowEvictedReactivation is the kill switch.
//
// Default ON, because the alternative is a permanently useless donor and a
// volunteer who did nothing wrong. An operator who wants eviction to be final
// sets it false and gets the previous behaviour, including the previous trap.
var AllowEvictedReactivation = true

// ReactivateEvicted brings an evicted donor back to active with no standing.
//
// It is called from the heartbeat path, which has already authenticated the
// caller and matched the certificate fingerprint. Both are re-checked here
// anyway: a function that undoes an eviction should not depend on its caller
// having done the checks.
func (s *Server) ReactivateEvicted(ctx context.Context, nodeID uuid.UUID,
	node gen.Node, presentedFingerprint string,
) error {
	if !AllowEvictedReactivation {
		return fmt.Errorf("%w: reactivation is disabled on this coordinator", ErrReactivationRefused)
	}
	if node.Status != gen.NodeStatusEvicted {
		return fmt.Errorf("%w: node is %s, not evicted", ErrReactivationRefused, node.Status)
	}
	if node.FederationCertFingerprint != presentedFingerprint {
		// Not the same donor. Eviction is about liveness; identity is still
		// identity, and a certificate that does not match is somebody else.
		return fmt.Errorf("%w: presented certificate is not the registered one",
			ErrReactivationRefused)
	}
	if node.TrustState == "revoked" {
		return fmt.Errorf("%w: trust was withdrawn from this node; that is not a liveness "+
			"judgement and reactivation cannot undo it", ErrReactivationRefused)
	}

	if err := s.q.ReactivateEvictedNode(ctx, pgUUIDFrom(nodeID)); err != nil {
		return err
	}

	payload, _ := json.Marshal(map[string]any{
		"node_id":     nodeID.String(),
		"from_status": string(node.Status),
		"reason": "the donor returned with its durable registration and matching certificate; " +
			"eviction was a liveness judgement and it is no longer true",
		"standing": "none — assignment sync forced to a full snapshot, no replica credited " +
			"until re-assigned and acknowledged or answered by a possession audit",
	})
	if err := s.q.InsertAuditLog(ctx, gen.InsertAuditLogParams{
		Action:     "node.reactivate.evicted",
		TargetType: "node",
		TargetID:   nodeID.String(),
		Payload:    payload,
	}); err != nil {
		// The state change already happened. Failing the heartbeat now would
		// leave the donor reactivated and told it was refused, which is worse
		// than an unrecorded reactivation — but it must be loud.
		return fmt.Errorf("coordinator: node %s was reactivated but the audit entry failed to "+
			"write: %w", nodeID, err)
	}
	return nil
}
