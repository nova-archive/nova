package possession

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
)

// TrustConfig holds the graduation thresholds (D-M6-8), all operator-tunable.
type TrustConfig struct {
	MinAge          time.Duration // default 7d
	MinPassedAudits int64         // default 10
	MinAckedXfers   int64         // default 5
	GraduateRep     float64       // default 0.95
}

// DefaultTrustConfig returns the D-M6-8 graduation defaults.
func DefaultTrustConfig() TrustConfig {
	return TrustConfig{
		MinAge:          7 * 24 * time.Hour,
		MinPassedAudits: 10,
		MinAckedXfers:   5,
		GraduateRep:     0.95,
	}
}

// applyTrust runs the automatic probationary<->trusted transitions. suspended is
// never set here (operator-only). Demotion is symmetric: trusted -> probationary
// below the floor. Called inside the outcome transaction so a graduation/demotion
// commits atomically with the reputation move that triggered it.
func (a *Auditor) applyTrust(ctx context.Context, q *gen.Queries, nodeID pgtype.UUID, score, floor float64) error {
	n, err := q.GetNodeTrust(ctx, nodeID)
	if err != nil {
		return err
	}
	nodeStr := uuid.UUID(nodeID.Bytes).String()
	switch n.TrustState {
	case "trusted":
		if score < floor {
			if err := q.SetTrustState(ctx, gen.SetTrustStateParams{ID: nodeID, TrustState: "probationary"}); err != nil {
				return err
			}
			slog.Info("audit.trust.demoted", "node", nodeStr, "reason", "below_floor", "score", score, "floor", floor)
			return nil
		}
	case "probationary":
		if n.TrustReviewRequiredAt.Valid {
			return nil // operator review gate (D-M6-2b)
		}
		if time.Since(n.TrustEpochStartedAt) < a.trust.MinAge {
			return nil
		}
		if score < a.trust.GraduateRep {
			return nil
		}
		epoch := pgtype.Timestamptz{Time: n.TrustEpochStartedAt, Valid: true}
		passed, err := q.CountPassedAuditsSince(ctx, gen.CountPassedAuditsSinceParams{NodeID: nodeID, DecidedAt: epoch})
		if err != nil {
			return err
		}
		xfers, err := q.CountAckedTransfersSince(ctx, gen.CountAckedTransfersSinceParams{NodeID: nodeID, AckedAt: epoch})
		if err != nil {
			return err
		}
		if passed >= a.trust.MinPassedAudits && xfers >= a.trust.MinAckedXfers {
			if err := q.SetTrustState(ctx, gen.SetTrustStateParams{ID: nodeID, TrustState: "trusted"}); err != nil {
				return err
			}
			slog.Info("audit.trust.graduated", "node", nodeStr, "score", score)
			return nil
		}
	}
	return nil
}
