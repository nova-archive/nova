package possession

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/notify"
)

// AuditTarget is the resolved challenge the coordinator dispatched and is now
// recording an outcome for. IDs are carried as strings (parsed to UUIDs here).
type AuditTarget struct {
	AuditID, NodeID, BlobCID, BlockCID, AssignmentID, Nonce string
	Generation, BlockIndex, BlockSize                       int64
	Deadline                                                time.Time
}

// Auditor records possession-audit outcomes and runs the reputation / durability
// / trust state machine. Coordinator-only.
type Auditor struct {
	pool     *pgxpool.Pool
	notifier notify.Notifier
	trust    TrustConfig
}

func NewAuditor(pool *pgxpool.Pool, n notify.Notifier, tc TrustConfig) *Auditor {
	if n == nil {
		n = notify.NoopNotifier{}
	}
	return &Auditor{pool: pool, notifier: n, trust: tc}
}

// Record applies the audit outcome in one transaction: revalidate the pin, record
// pin_audits, move reputation (FOR UPDATE row-locked), correct durability on a hard
// fail, and run the trust state machine. A post-commit federation.node_suspect
// fires on a hash mismatch.
func (a *Auditor) Record(ctx context.Context, t AuditTarget, res DispatchResult, reputationFloor float64) error {
	auditID, err := pgUUID(t.AuditID)
	if err != nil {
		return fmt.Errorf("audit_id: %w", err)
	}
	nodeID, err := pgUUID(t.NodeID)
	if err != nil {
		return fmt.Errorf("node_id: %w", err)
	}
	assignmentID, err := pgUUID(t.AssignmentID)
	if err != nil {
		return fmt.Errorf("assignment_id: %w", err)
	}

	var suspect bool
	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		q := gen.New(tx)
		decided := time.Now()

		// D-M6-7a: stale-challenge guard. If the audited assignment is no longer the
		// live acked one, record a skip and move nothing.
		still, err := q.RevalidateAuditPin(ctx, gen.RevalidateAuditPinParams{
			Cid: t.BlobCID, NodeID: nodeID, AssignmentID: assignmentID, Generation: t.Generation,
		})
		if err != nil {
			return err
		}
		if !still {
			_, err := q.RecordAuditOutcome(ctx, recordParams(auditID, "skip", nil, decided, res, "stale_challenge"))
			return err
		}

		result, repMul, repZero, hard, mismatch, errReason := classify(res.Outcome)
		var transcript []byte
		if res.Outcome == OutcomePass {
			transcript = wire.AuditTranscriptHash(wire.AuditChallenge{
				ChallengeID: t.AuditID, BlobCID: t.BlobCID, AssignmentID: t.AssignmentID, Generation: t.Generation,
				BlockCID: t.BlockCID, BlockIndex: t.BlockIndex, BlockSize: t.BlockSize, Nonce: t.Nonce,
			}, res.Bytes)
		}
		// RecordAuditOutcome resolves only the unresolved (result IS NULL) row, so a
		// replayed challenge_id cannot silently overwrite a decided audit: 0 rows means
		// already-decided/replay, and we must not re-apply any movement.
		n, err := q.RecordAuditOutcome(ctx, recordParams(auditID, result, transcript, decided, res, errReason))
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if result == "skip" {
			return nil // budget/unreachable skip: no movement
		}

		// Row-lock the node so the read-compute-write reputation update cannot lose a
		// concurrent update (Blocker 5).
		cur, err := q.GetNodeTrustForUpdate(ctx, nodeID)
		if err != nil {
			return err
		}
		var newScore float32
		switch {
		case repZero:
			newScore = 0
		case repMul > 0:
			newScore = cur.ReputationScore * float32(repMul)
		default:
			newScore = minF32(1, cur.ReputationScore+0.01)
		}
		if _, err := q.MoveReputation(ctx, gen.MoveReputationParams{ID: nodeID, Column2: newScore}); err != nil {
			return err
		}
		slog.Info("audit.reputation.moved", "node", t.NodeID, "from", cur.ReputationScore, "to", newScore, "outcome", result)

		if hard {
			// FailAckedPinAssignmentForAudit fails ONLY the acked row for this exact
			// assignment/generation (M5's FailPinAssignment fails pending rows only).
			failed, err := q.FailAckedPinAssignmentForAudit(ctx, gen.FailAckedPinAssignmentForAuditParams{
				Cid: t.BlobCID, NodeID: nodeID, AssignmentID: assignmentID, Generation: t.Generation,
			})
			if err != nil {
				return err
			}
			if failed == 1 { // only enqueue if we actually invalidated the live acked pin
				if err := q.EnqueueReconcile(ctx, gen.EnqueueReconcileParams{
					Cid: t.BlobCID, Reason: reconcileReason(mismatch),
				}); err != nil {
					return err
				}
			}
		}
		if mismatch {
			if err := q.SetTrustReview(ctx, gen.SetTrustReviewParams{ID: nodeID, TrustReviewReason: pgText("hash_mismatch")}); err != nil {
				return err
			}
			suspect = true
		}
		// Below-floor BULK re-replication is intentionally NOT done here (deferred to
		// P2-M7, D-M6-7): below-floor excludes new placement + deprioritizes source
		// ordering, but present acked pins stay countable unless a pin-specific hard
		// failure invalidated one above.
		return a.applyTrust(ctx, q, nodeID, float64(newScore), reputationFloor)
	})
	if err != nil {
		return err
	}
	if suspect {
		a.notifier.Emit(ctx, notify.Event{Type: "federation.node_suspect", ScopeKey: t.NodeID,
			Payload: map[string]any{"node_id": t.NodeID, "reason": "hash_mismatch", "affected_blob_cid": t.BlobCID, "audit_id": t.AuditID}})
	}
	return nil
}

// classify maps a dispatch outcome to (db result, repMultiplier, repZero, hardFail, mismatch, errReason).
func classify(o Outcome) (result string, mul float64, zero, hard, mismatch bool, reason string) {
	switch o {
	case OutcomePass:
		return "pass", 0, false, false, false, ""
	case OutcomeFailDeadline:
		return "fail", 0.95, false, false, false, "deadline"
	case OutcomeFailNotPresent:
		return "fail", 0.5, false, true, false, "not_present"
	case OutcomeFailMismatch:
		return "fail", 0, true, true, true, "mismatch"
	case OutcomeSkipBudget:
		return "skip", 0, false, false, false, "audit_budget_exhausted"
	case OutcomeSkipUnreachable:
		return "skip", 0, false, false, false, "unreachable"
	}
	return "skip", 0, false, false, false, "unknown"
}

func reconcileReason(mismatch bool) string {
	if mismatch {
		return "audit_mismatch"
	}
	return "audit_not_present"
}

// recordParams builds RecordAuditOutcomeParams for the given decision. received_at
// is NULL when the donor was never reached (D10); latency/bytes/transcript are set
// only on a pass.
func recordParams(auditID pgtype.UUID, result string, transcript []byte, decided time.Time, res DispatchResult, reason string) gen.RecordAuditOutcomeParams {
	p := gen.RecordAuditOutcomeParams{
		ID:             auditID,
		Result:         gen.NullAuditResult{AuditResult: gen.AuditResult(result), Valid: true},
		DecidedAt:      pgtype.Timestamptz{Time: decided, Valid: true},
		TranscriptHash: transcript,
		Error:          pgtype.Text{String: reason, Valid: reason != ""},
	}
	if !res.ReceivedAt.IsZero() {
		p.ReceivedAt = pgtype.Timestamptz{Time: res.ReceivedAt, Valid: true}
	}
	if result == "pass" || res.LatencyMS > 0 {
		p.LatencyMs = pgtype.Int4{Int32: int32(res.LatencyMS), Valid: true}
	}
	if result == "pass" {
		p.BytesVerified = pgtype.Int8{Int64: int64(len(res.Bytes)), Valid: true}
	}
	return p
}

// pgUUID parses a canonical UUID string into the pgtype representation. A malformed
// UUID surfaces as an error rather than silently becoming the zero UUID.
func pgUUID(s string) (pgtype.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}

func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func minF32(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}
