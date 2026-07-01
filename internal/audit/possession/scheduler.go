package possession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// SchedulerConfig is the self-contained configuration for the possession audit
// scheduler. Task 13 will populate it from config.PossessionAudit.Effective*().
type SchedulerConfig struct {
	NewAckWindow      time.Duration // fast-lane: pins acked within this window
	FastLaneQuota     int           // max fast-lane challenges per tick
	NodesPerTick      int           // baseline: SelectDueAuditNodes LIMIT
	MaxBlockBytes     int64         // never challenge a block larger than this
	Deadline          time.Duration // per-challenge timeout AND pin_audits.deadline horizon
	ReputationFloor   float64       // passed to Auditor.Record
	StaleGraceSeconds float64       // ReconcileStaleAudits grace period
	BaseInterval      time.Duration // baseline per-node cadence (modulated by due())
}

// challenger is the interface the Scheduler uses to dispatch audit challenges.
// *Dispatcher from Task 7 satisfies it. Tests inject a fake.
type challenger interface {
	Challenge(ctx context.Context, addr string, ch wire.AuditChallenge) (DispatchResult, error)
}

// Scheduler runs the two-stage possession audit sampling loop in-process on the
// coordinator. It is NOT backed by the persistent job queue; on restart it
// reconciles stale challenges and resumes from each node's last recorded audit.
type Scheduler struct {
	pool     *pgxpool.Pool
	dispatch challenger
	auditor  *Auditor
	cfg      SchedulerConfig
	now      func() time.Time
	tick     time.Duration

	mu      sync.Mutex
	lastRun map[string]time.Time
}

const defaultSchedulerTick = 30 * time.Second

// SchedulerOption tunes a Scheduler (primarily for tests).
type SchedulerOption func(*Scheduler)

// WithClock overrides the time source.
func WithClock(now func() time.Time) SchedulerOption {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// WithTick overrides the scheduler tick interval.
func WithTick(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		if d > 0 {
			s.tick = d
		}
	}
}

// NewScheduler constructs a Scheduler.
func NewScheduler(pool *pgxpool.Pool, dispatch challenger, auditor *Auditor, cfg SchedulerConfig, opts ...SchedulerOption) *Scheduler {
	s := &Scheduler{
		pool:     pool,
		dispatch: dispatch,
		auditor:  auditor,
		cfg:      cfg,
		now:      time.Now,
		tick:     defaultSchedulerTick,
		lastRun:  make(map[string]time.Time),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Run calls ReconcileOnStartup then ticks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	if err := s.ReconcileOnStartup(ctx); err != nil {
		slog.Warn("audit.possession.scheduler.reconcile_failed", "err", err)
	}
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runOnce(ctx)
		}
	}
}

// ReconcileOnStartup performs two startup actions (D-M6-5 + D-M6-3b):
//  1. Crashed-mid-flight challenges (result IS NULL past deadline) -> skip.
//  2. Seed per-node lastRun from the last resolved audit so a restart does not
//     immediately re-audit every due node at once.
func (s *Scheduler) ReconcileOnStartup(ctx context.Context) error {
	q := gen.New(s.pool)

	// Step 1: mark stale in-flight challenges as skipped.
	if err := q.ReconcileStaleAudits(ctx, s.cfg.StaleGraceSeconds); err != nil {
		return err
	}

	// Step 2: seed per-node cadence from last decided audit.
	rows, err := q.SelectLastAuditPerNode(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, r := range rows {
		s.lastRun[uuid.UUID(r.NodeID.Bytes).String()] = r.LastDecidedAt
	}
	s.mu.Unlock()
	return nil
}

// runOnce executes one scheduler tick: fast-lane (newly-acked pins within the
// window, quota-bounded) then baseline (due nodes, one size-weighted pin each).
func (s *Scheduler) runOnce(ctx context.Context) {
	q := gen.New(s.pool)

	// Fast lane: pins acked within the window that have never been audited.
	fast, _ := q.SelectNewlyAckedPins(ctx, gen.SelectNewlyAckedPinsParams{
		AckedAt: pgtype.Timestamptz{Time: s.now().Add(-s.cfg.NewAckWindow), Valid: true},
		Limit:   int32(s.cfg.FastLaneQuota),
	})
	for _, p := range fast {
		s.auditOne(ctx, q,
			uuid.UUID(p.NodeID.Bytes).String(),
			p.Cid,
			uuid.UUID(p.AssignmentID.Bytes).String(),
			p.Generation,
		)
	}

	// Baseline: due nodes with at least one acked pin, ordered by pressure.
	nodes, _ := q.SelectDueAuditNodes(ctx, int32(s.cfg.NodesPerTick))
	for _, n := range nodes {
		if !s.due(n) {
			continue
		}
		pin, err := q.SelectAckedPinForAudit(ctx, n.NodeID)
		if err != nil {
			continue
		}
		nodeKey := uuid.UUID(n.NodeID.Bytes).String()
		s.auditOne(ctx, q,
			nodeKey,
			pin.Cid,
			uuid.UUID(pin.AssignmentID.Bytes).String(),
			pin.Generation,
		)
		// Record lastRun after auditing so cadence is anchored to the actual audit time.
		s.mu.Lock()
		s.lastRun[nodeKey] = s.now()
		s.mu.Unlock()
	}
}

// due implements cadence modulation (D-M6-5b): trusted & rep >= 0.95 nodes are
// audited less often (×1.25 interval); probationary or rep < 0.5 are audited
// more often (×0.25 interval).
func (s *Scheduler) due(n gen.SelectDueAuditNodesRow) bool {
	var multiplier float64
	switch {
	case n.TrustState == "trusted" && float64(n.ReputationScore) >= 0.95:
		multiplier = 1.25
	case n.TrustState == "probationary" || float64(n.ReputationScore) < 0.5:
		multiplier = 0.25
	default:
		multiplier = 1.0
	}
	effectiveInterval := time.Duration(float64(s.cfg.BaseInterval) * multiplier)
	nodeKey := uuid.UUID(n.NodeID.Bytes).String()
	s.mu.Lock()
	last := s.lastRun[nodeKey]
	s.mu.Unlock()
	return s.now().Sub(last) >= effectiveInterval
}

// auditOne resolves one (node, blob-cid, assignment) triple into a full audit
// cycle: select a random in-cap block, insert the challenge, dispatch, record.
func (s *Scheduler) auditOne(ctx context.Context, q *gen.Queries, nodeIDStr, cid, assignmentID string, generation int64) {
	// Stage 3: pick a random block within the size cap.
	blk, err := q.SelectRandomBlockForCID(ctx, gen.SelectRandomBlockForCIDParams{
		BlobCid:   cid,
		BlockSize: int32(s.cfg.MaxBlockBytes),
	})
	if err != nil {
		slog.Info("audit.possession.skipped", "reason", "no_eligible_block", "cid", cid)
		return
	}

	addr, ok := s.donorAddr(ctx, q, nodeIDStr)
	if !ok {
		return
	}

	target := AuditTarget{
		AuditID:      uuidNew(),
		NodeID:       nodeIDStr,
		BlobCID:      cid,
		BlockCID:     blk.BlockCid,
		AssignmentID: assignmentID,
		Generation:   generation,
		Nonce:        randNonce(),
		BlockIndex:   int64(blk.BlockIndex),
		BlockSize:    int64(blk.BlockSize),
		Deadline:     s.now().Add(s.cfg.Deadline),
	}

	// Build pgtype IDs; if somehow malformed, skip this audit.
	auditIDPg, err := pgUUID(target.AuditID)
	if err != nil {
		return
	}
	nodeIDPg, err := pgUUID(target.NodeID)
	if err != nil {
		return
	}

	// Insert-before-dispatch (D-M6-3b): crash mid-flight leaves a NULL result
	// row that ReconcileOnStartup will clean up on next start.
	if err := q.InsertAuditChallenge(ctx, gen.InsertAuditChallengeParams{
		ID:            auditIDPg,
		BlobCid:       target.BlobCID,
		NodeID:        nodeIDPg,
		ChallengeKind: wire.AuditChallengeKindBlockHash,
		Nonce:         target.Nonce,
		Deadline:      target.Deadline,
	}); err != nil {
		slog.Warn("audit.possession.insert_failed", "cid", cid, "err", err)
		return
	}
	slog.Info("audit.possession.challenged", "node", nodeIDStr, "cid", target.BlobCID, "block", target.BlockCID)

	// Dispatch with per-challenge timeout.
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Deadline)
	defer cancel()
	res, _ := s.dispatch.Challenge(cctx, addr, wire.AuditChallenge{
		ChallengeID:  target.AuditID,
		BlobCID:      cid,
		AssignmentID: assignmentID,
		Generation:   generation,
		BlockIndex:   int64(blk.BlockIndex),
		BlockCID:     blk.BlockCid,
		BlockSize:    int64(blk.BlockSize),
		Nonce:        target.Nonce,
	})

	if err := s.auditor.Record(ctx, target, res, s.cfg.ReputationFloor); err != nil {
		slog.Warn("audit.possession.record_error", "cid", cid, "err", err)
	}
	s.logOutcome(cid, nodeIDStr, res)
}

// donorAddr fetches the donor's source nebula address. Returns ("", false) if
// the node has no address configured or the query fails.
func (s *Scheduler) donorAddr(ctx context.Context, q *gen.Queries, nodeIDStr string) (string, bool) {
	nodeID, err := pgUUID(nodeIDStr)
	if err != nil {
		return "", false
	}
	addr, err := q.GetNodeSourceAddr(ctx, nodeID)
	if err != nil || !addr.Valid || addr.String == "" {
		return "", false
	}
	return addr.String, true
}

func (s *Scheduler) logOutcome(cid, nodeID string, res DispatchResult) {
	switch res.Outcome {
	case OutcomePass:
		slog.Info("audit.possession.passed", "node", nodeID, "cid", cid)
	case OutcomeFailDeadline:
		slog.Info("audit.possession.failed", "node", nodeID, "cid", cid, "reason", "deadline")
	case OutcomeFailNotPresent:
		slog.Info("audit.possession.failed", "node", nodeID, "cid", cid, "reason", "not_present")
	case OutcomeFailMismatch:
		slog.Info("audit.possession.failed", "node", nodeID, "cid", cid, "reason", "mismatch")
	case OutcomeSkipBudget:
		slog.Info("audit.possession.skipped", "node", nodeID, "cid", cid, "reason", "budget_exhausted")
		slog.Info("audit.governor.exhausted", "node", nodeID, "cid", cid)
	case OutcomeSkipUnreachable:
		slog.Info("audit.possession.skipped", "node", nodeID, "cid", cid, "reason", "unreachable")
	default:
		slog.Info("audit.possession.skipped", "node", nodeID, "cid", cid, "reason", "unknown")
	}
}

// uuidNew generates a new random UUID string.
func uuidNew() string { return uuid.NewString() }

// randNonce returns a 16-byte cryptographically random hex-encoded nonce.
func randNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to UUID bytes on the vanishingly rare rand failure.
		return uuid.NewString()
	}
	return hex.EncodeToString(b)
}
