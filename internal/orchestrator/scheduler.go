package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	coordinator "github.com/nova-archive/nova/internal/federation/coordinator"
	"github.com/nova-archive/nova/pkg/coordinator/admission"
)

// SchedulerConfig tunes one healing tick.
type SchedulerConfig struct {
	Targets         ReplicationTargets
	ReputationFloor float64
	DrainBatch      int // reconcile-queue drain bound per tick
	TierLimit       int // max CIDs healed per tier per tick
}

// Scheduler reserves repair replicas for under-replicated CIDs, strict Tier-1
// first (D-M5-6). It holds no in-memory tier state: every tick re-derives the
// work set from the projection, so a restart resumes cleanly.
type Scheduler struct {
	pool *pgxpool.Pool
	cfg  SchedulerConfig
}

// NewScheduler builds a Scheduler with sane bounds for any non-positive knob.
func NewScheduler(pool *pgxpool.Pool, cfg SchedulerConfig) *Scheduler {
	if cfg.DrainBatch <= 0 {
		cfg.DrainBatch = 256
	}
	if cfg.TierLimit <= 0 {
		cfg.TierLimit = 256
	}
	return &Scheduler{pool: pool, cfg: cfg}
}

// Tick runs one healing pass: drain the reconcile queue (bounded), heal Tier-1
// CIDs, and — only when Tier-1 was empty this tick (strict Tier-1) — heal Tier-2
// toward target_count. Returns the number of reservations made.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	if _, err := DrainReconcile(ctx, s.pool, s.cfg.DrainBatch, s.cfg.Targets); err != nil {
		return 0, err
	}
	q := gen.New(s.pool)
	tier1, err := q.ListUnderReplicatedByTier(ctx, gen.ListUnderReplicatedByTierParams{SafetyTier: "tier1", Limit: int32(s.cfg.TierLimit)})
	if err != nil {
		return 0, err
	}
	healed := 0
	for _, row := range tier1 {
		ok, err := s.healCID(ctx, row.Cid)
		if err != nil {
			return healed, err
		}
		if ok {
			healed++
		}
	}
	// Strict Tier-1: descend to Tier-2 only when no Tier-1 work existed this tick.
	if len(tier1) == 0 {
		tier2, err := q.ListUnderReplicatedByTier(ctx, gen.ListUnderReplicatedByTierParams{SafetyTier: "tier2", Limit: int32(s.cfg.TierLimit)})
		if err != nil {
			return healed, err
		}
		for _, row := range tier2 {
			ok, err := s.healCID(ctx, row.Cid)
			if err != nil {
				return healed, err
			}
			if ok {
				healed++
			}
		}
	}
	return healed, nil
}

// healCID reserves at most one new replica for cid in a single transaction. It
// recomputes the projection from authority FIRST (never schedule from a stale or
// dirty row, D-M5-2d), bails if the CID is no longer under-replicated, then selects
// a repair source (best repair-sourceable holder, else the coordinator when
// local-recoverable, else donor_lost ⇒ skip) and a destination (Task-4 engine),
// reserves via AssignPinWithSource, and re-recomputes in-tx. Returns true iff a
// reservation was made.
func (s *Scheduler) healCID(ctx context.Context, cid string) (bool, error) {
	// A CID with a pending reservation in source-retry backoff is left alone so a
	// flapping source is not re-picked (Rev. 5 #3).
	inBackoff, err := gen.New(s.pool).CIDHasPendingInBackoff(ctx, cid)
	if err != nil {
		return false, err
	}
	if inBackoff {
		return false, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	if err := RecomputeCID(ctx, tx, cid, s.cfg.Targets); err != nil {
		return false, err
	}
	proj, err := q.GetReplicationState(ctx, cid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if int(proj.HealthyAckedCount) >= int(proj.TargetCount) {
		return false, nil // no longer under-replicated
	}

	size, err := q.GetBlobSize(ctx, cid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	source := uuid.Nil
	srcRow, err := q.ListRepairSourceHolders(ctx, gen.ListRepairSourceHoldersParams{Cid: cid, Size: pgtype.Int8{Int64: size, Valid: true}})
	switch {
	case err == nil:
		source = uuid.UUID(srcRow.NodeID.Bytes)
	case errors.Is(err, pgx.ErrNoRows):
		if !proj.LocalRecoverable {
			return false, nil // donor_lost ∧ ¬local_recoverable: do not spin the queue
		}
		// source stays uuid.Nil ⇒ coordinator-as-source emergency path (D-M5-8b).
	default:
		return false, err
	}

	dest, ok, err := s.selectDest(ctx, q, cid, proj.DurabilityClass, size)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil // no eligible destination this tick
	}

	if _, err := coordinator.AssignPinWithSource(ctx, tx, cid, dest, source); err != nil {
		if errors.Is(err, coordinator.ErrSourceNotSourceable) || errors.Is(err, coordinator.ErrSourceIsDest) {
			return false, nil // raced with a liveness change; next tick re-picks
		}
		return false, err
	}
	if err := RecomputeCID(ctx, tx, cid, s.cfg.Targets); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	srcLabel := "coordinator"
	if source != uuid.Nil {
		srcLabel = source.String()
	}
	slog.Info("heal.scheduled", "cid", cid, "dest", dest, "source", srcLabel, "target", proj.TargetCount)
	return true, nil
}

// selectDest maps the eligible candidates + existing holders into the pure Task-4
// placement engine and returns its choice.
func (s *Scheduler) selectDest(ctx context.Context, q *gen.Queries, cid, class string, size int64) (uuid.UUID, bool, error) {
	holders, err := q.ListCIDHolders(ctx, cid)
	if err != nil {
		return uuid.Nil, false, err
	}
	cands, err := q.ListPlacementCandidates(ctx, cid)
	if err != nil {
		return uuid.Nil, false, err
	}
	hs := make([]admission.Holder, len(holders))
	for i, h := range holders {
		hs[i] = admission.Holder{
			FailureDomain: h.FailureDomainID.String, Principal: h.DonorPrincipalID.String,
			Provider: h.Provider.String, ASN: h.Asn.String, Region: h.Region.String,
			OperatorVerified: h.OperatorVerified,
		}
	}
	cs := make([]admission.Candidate, len(cands))
	for i, c := range cands {
		cs[i] = admission.Candidate{
			NodeID: uuid.UUID(c.NodeID.Bytes), FailureDomain: c.FailureDomainID.String,
			Principal: c.DonorPrincipalID.String, Provider: c.Provider.String, ASN: c.Asn.String,
			Region: c.Region.String, OperatorVerified: c.OperatorVerified, FreeBytes: c.FreeBytes,
			TrustState: c.TrustState, Reputation: float64(c.ReputationScore), PlacementWeight: float64(c.PlacementWeight),
		}
	}
	dest, ok := admission.SelectDestination(class, size, hs, cs, s.cfg.ReputationFloor)
	return dest, ok, nil
}

// WarnIfImportantBelowFive emits the warn-not-force advisory when the important
// replication factor is below the durability recommendation (D-M5-12). It returns
// the warning text (empty if none) so cmd/coordinator can surface it alongside the
// privacy warnings.
func WarnIfImportantBelowFive(important int) string {
	if important < 5 {
		slog.Warn("orchestrator.replication.important_below_recommended", "important", important)
		return fmt.Sprintf("replication.factor.important=%d is below the recommended 5; important originals are less durable", important)
	}
	return ""
}
