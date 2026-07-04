package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/prometheus/client_golang/prometheus"
)

// dbCollector reads durable projections at scrape time (D-M7-1a):
//
//	nova_replication_cids{tier,class}            blob_replication_state
//	nova_reconcile_queue_depth{reason}           blob_replication_reconcile_queue
//	nova_reconcile_queue_oldest_seconds{reason}  ...
//	nova_nodes{status,trust_state,assignment_sync_state}
//	nova_below_floor_replica_debt / nova_node_below_floor_replicas{node_id}   CountBelowFloorReplicas
//	nova_node_draining / _drain_pending_cids / _drain_inflight_cids /
//	_drain_pending_oldest_seconds / _drain_ready {node_id}                    ListDrainingNodes + Count* per node
//	nova_audit_results_total{result,reason}      durable pin_audits rows (restart-stable counter)
//
// On a query error the family is logged at warn and skipped — a broken scrape
// must never panic the handler. The drain families iterate per draining node;
// that population is operator-initiated and tiny by construction (D-M7-6).
type dbCollector struct {
	pool  *pgxpool.Pool
	floor float64

	replicationCIDs   *prometheus.Desc
	queueDepth        *prometheus.Desc
	queueOldest       *prometheus.Desc
	nodes             *prometheus.Desc
	belowFloorTotal   *prometheus.Desc
	belowFloorPerNode *prometheus.Desc
	draining          *prometheus.Desc
	drainPending      *prometheus.Desc
	drainInflight     *prometheus.Desc
	drainOldest       *prometheus.Desc
	drainReady        *prometheus.Desc
	auditResults      *prometheus.Desc
}

func newDBCollector(pool *pgxpool.Pool, reputationFloor float64) *dbCollector {
	return &dbCollector{
		pool:  pool,
		floor: reputationFloor,
		replicationCIDs: prometheus.NewDesc("nova_replication_cids",
			"CIDs by replication safety tier and durability class.", []string{"tier", "class"}, nil),
		queueDepth: prometheus.NewDesc("nova_reconcile_queue_depth",
			"Reconcile-queue entries by reason.", []string{"reason"}, nil),
		queueOldest: prometheus.NewDesc("nova_reconcile_queue_oldest_seconds",
			"Age of the oldest reconcile-queue entry by reason.", []string{"reason"}, nil),
		nodes: prometheus.NewDesc("nova_nodes",
			"Nodes by status, trust state and assignment sync state.",
			[]string{"status", "trust_state", "assignment_sync_state"}, nil),
		belowFloorTotal: prometheus.NewDesc("nova_below_floor_replica_debt",
			"Acked, countable replicas held on nodes below the reputation floor (observability only; remedy is P2-M7.1).", nil, nil),
		belowFloorPerNode: prometheus.NewDesc("nova_node_below_floor_replicas",
			"Acked, countable replicas on this below-floor node.", []string{"node_id"}, nil),
		draining: prometheus.NewDesc("nova_node_draining",
			"1 for each draining node (D-M7-6).", []string{"node_id"}, nil),
		drainPending: prometheus.NewDesc("nova_node_drain_pending_cids",
			"Drain debt: CIDs still below target without this draining node (D-M7-6f).", []string{"node_id"}, nil),
		drainInflight: prometheus.NewDesc("nova_node_drain_inflight_cids",
			"Drain-debt CIDs with a pending replacement on an eligible destination.", []string{"node_id"}, nil),
		drainOldest: prometheus.NewDesc("nova_node_drain_pending_oldest_seconds",
			"Seconds since drain started while debt remains (0 once drain-ready).", []string{"node_id"}, nil),
		drainReady: prometheus.NewDesc("nova_node_drain_ready",
			"1 when this draining node has zero drain debt (safe-to-revoke gate input).", []string{"node_id"}, nil),
		auditResults: prometheus.NewDesc("nova_audit_results_total",
			"Possession-audit outcomes by result and reason (durable pin_audits rows; restart-stable).",
			[]string{"result", "reason"}, nil),
	}
}

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.replicationCIDs, c.queueDepth, c.queueOldest, c.nodes,
		c.belowFloorTotal, c.belowFloorPerNode,
		c.draining, c.drainPending, c.drainInflight, c.drainOldest, c.drainReady,
		c.auditResults,
	} {
		ch <- d
	}
}

func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	c.collectGroupBy(ctx, ch, c.replicationCIDs, prometheus.GaugeValue,
		`SELECT safety_tier::text, durability_class::text, count(*)::float8
		 FROM blob_replication_state GROUP BY 1, 2`, 2)
	c.collectQueue(ctx, ch)
	c.collectGroupBy(ctx, ch, c.nodes, prometheus.GaugeValue,
		`SELECT status::text, trust_state::text, assignment_sync_state, count(*)::float8
		 FROM nodes GROUP BY 1, 2, 3`, 3)
	c.collectBelowFloor(ctx, ch)
	c.collectDrain(ctx, ch)
	c.collectGroupBy(ctx, ch, c.auditResults, prometheus.CounterValue,
		`SELECT COALESCE(result::text, 'pending'), COALESCE(NULLIF(error, ''), 'none'), count(*)::float8
		 FROM pin_audits GROUP BY 1, 2`, 2)
}

// collectGroupBy emits one metric per row of a "label..., value" GROUP BY query.
func (c *dbCollector) collectGroupBy(ctx context.Context, ch chan<- prometheus.Metric,
	desc *prometheus.Desc, vt prometheus.ValueType, sql string, labelCount int) {
	rows, err := c.pool.Query(ctx, sql)
	if err != nil {
		slog.Warn("metrics.scrape_query_failed", "desc", desc.String(), "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		labels := make([]string, labelCount)
		ptrs := make([]any, 0, labelCount+1)
		for i := range labels {
			ptrs = append(ptrs, &labels[i])
		}
		var v float64
		ptrs = append(ptrs, &v)
		if err := rows.Scan(ptrs...); err != nil {
			slog.Warn("metrics.scrape_scan_failed", "desc", desc.String(), "err", err)
			return
		}
		ch <- prometheus.MustNewConstMetric(desc, vt, v, labels...)
	}
}

func (c *dbCollector) collectQueue(ctx context.Context, ch chan<- prometheus.Metric) {
	rows, err := c.pool.Query(ctx,
		`SELECT reason, count(*)::float8,
		        COALESCE(EXTRACT(EPOCH FROM (now() - min(enqueued_at))), 0)::float8
		 FROM blob_replication_reconcile_queue GROUP BY reason`)
	if err != nil {
		slog.Warn("metrics.scrape_query_failed", "family", "nova_reconcile_queue_depth", "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		var depth, oldest float64
		if err := rows.Scan(&reason, &depth, &oldest); err != nil {
			slog.Warn("metrics.scrape_scan_failed", "family", "nova_reconcile_queue_depth", "err", err)
			return
		}
		ch <- prometheus.MustNewConstMetric(c.queueDepth, prometheus.GaugeValue, depth, reason)
		ch <- prometheus.MustNewConstMetric(c.queueOldest, prometheus.GaugeValue, oldest, reason)
	}
}

func (c *dbCollector) collectBelowFloor(ctx context.Context, ch chan<- prometheus.Metric) {
	rows, err := gen.New(c.pool).CountBelowFloorReplicas(ctx, c.floor)
	if err != nil {
		slog.Warn("metrics.scrape_query_failed", "family", "nova_below_floor_replica_debt", "err", err)
		return
	}
	var total float64
	for _, r := range rows {
		v := float64(r.AckedReplicas)
		total += v
		ch <- prometheus.MustNewConstMetric(c.belowFloorPerNode, prometheus.GaugeValue, v,
			uuid.UUID(r.NodeID.Bytes).String())
	}
	ch <- prometheus.MustNewConstMetric(c.belowFloorTotal, prometheus.GaugeValue, total)
}

func (c *dbCollector) collectDrain(ctx context.Context, ch chan<- prometheus.Metric) {
	q := gen.New(c.pool)
	nodes, err := q.ListDrainingNodes(ctx)
	if err != nil {
		slog.Warn("metrics.scrape_query_failed", "family", "nova_node_draining", "err", err)
		return
	}
	now := time.Now()
	for _, n := range nodes {
		id := uuid.UUID(n.ID.Bytes).String()
		ch <- prometheus.MustNewConstMetric(c.draining, prometheus.GaugeValue, 1, id)
		pending, err := q.CountDrainPendingCIDs(ctx, n.ID)
		if err != nil {
			slog.Warn("metrics.scrape_query_failed", "family", "nova_node_drain_pending_cids", "err", err)
			continue
		}
		inflight, err := q.CountDrainInflightCIDs(ctx, n.ID)
		if err != nil {
			slog.Warn("metrics.scrape_query_failed", "family", "nova_node_drain_inflight_cids", "err", err)
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.drainPending, prometheus.GaugeValue, float64(pending), id)
		ch <- prometheus.MustNewConstMetric(c.drainInflight, prometheus.GaugeValue, float64(inflight), id)
		oldest, ready := 0.0, 1.0
		if pending > 0 {
			oldest, ready = now.Sub(n.DrainingAt.Time).Seconds(), 0.0
		}
		ch <- prometheus.MustNewConstMetric(c.drainOldest, prometheus.GaugeValue, oldest, id)
		ch <- prometheus.MustNewConstMetric(c.drainReady, prometheus.GaugeValue, ready, id)
	}
}
