// Package metrics is the P2-M7 (D-M7-1) coordinator-only observability surface.
// Two source kinds (D-M7-1a): DB-derived gauges/counters computed at scrape time
// from durable projections (collector.go), and process-local counters/histograms
// instrumented at event sites via the Observe* hooks — those reset on restart,
// which is normal Prometheus counter behavior; do NOT "fix" a reset by inventing
// a durable event table. Label discipline: bounded label sets only; NEVER
// per-CID/blob/path/filename labels (enforced by TestLabelDiscipline).
package metrics

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	reg *prometheus.Registry

	registerFailures  *prometheus.CounterVec // reason
	trustTransitions  *prometheus.CounterVec // from,to,reason
	reputationMoved   *prometheus.CounterVec // direction
	auditLatency      prometheus.Histogram
	donorFetch        *prometheus.CounterVec // result,reason
	donorFetchLatency prometheus.Histogram
	egressRefusals    *prometheus.CounterVec // reason
	sourceSelFailures *prometheus.CounterVec // reason
	belowFloorRequeue prometheus.Counter     // CIDs requeued by the below-floor sweep
}

// New builds the registry. belowFloorGraceSecs is the P2-M7.1
// below_floor_replacement.grace window (seconds) the scrape-time drain-debt
// and below-floor families evaluate sustained markers against.
func New(pool *pgxpool.Pool, reputationFloor, belowFloorGraceSecs float64) *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}
	m.registerFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_compat_registration_failures_total",
		Help: "fed/v1 register rejections by reason (process-local; resets on restart).",
	}, []string{"reason"})
	m.trustTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_trust_transitions_total",
		Help: "Trust state transitions (process-local).",
	}, []string{"from", "to", "reason"})
	m.reputationMoved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_reputation_moved_total",
		Help: "Reputation movements by direction (process-local).",
	}, []string{"direction"})
	m.auditLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "nova_audit_latency_seconds", Help: "Possession-audit round-trip latency.",
		Buckets: prometheus.DefBuckets,
	})
	m.donorFetch = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_donor_fetch_total", Help: "Donor-backed read fetches by outcome (process-local).",
	}, []string{"result", "reason"})
	m.donorFetchLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "nova_donor_fetch_latency_seconds", Help: "Donor-backed read fetch latency.",
		Buckets: prometheus.DefBuckets,
	})
	m.egressRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_donor_egress_refusals_total", Help: "Donor egress-budget refusals observed by the coordinator.",
	}, []string{"reason"})
	m.sourceSelFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nova_source_selection_failures_total", Help: "Read/repair source selection failures by reason.",
	}, []string{"reason"})
	m.belowFloorRequeue = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nova_below_floor_requeued_total",
		Help: "CIDs enqueued for re-replication by the below-floor sweep (process-local; resets on restart).",
	})
	m.reg.MustRegister(m.registerFailures, m.trustTransitions, m.reputationMoved,
		m.auditLatency, m.donorFetch, m.donorFetchLatency, m.egressRefusals,
		m.sourceSelFailures, m.belowFloorRequeue,
		newDBCollector(pool, reputationFloor, belowFloorGraceSecs))
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Hook methods — the ONLY coupling event sites have to this package is a
// func value / tiny interface (Task 5); they never import prometheus.
func (m *Metrics) ObserveRegisterFailure(reason string) {
	m.registerFailures.WithLabelValues(reason).Inc()
}
func (m *Metrics) ObserveTrustTransition(from, to, reason string) {
	m.trustTransitions.WithLabelValues(from, to, reason).Inc()
}
func (m *Metrics) ObserveReputationMove(direction string) {
	m.reputationMoved.WithLabelValues(direction).Inc()
}
func (m *Metrics) ObserveAuditLatency(sec float64) { m.auditLatency.Observe(sec) }
func (m *Metrics) ObserveDonorFetch(result, reason string, sec float64) {
	m.donorFetch.WithLabelValues(result, reason).Inc()
	m.donorFetchLatency.Observe(sec)
}
func (m *Metrics) ObserveEgressRefusal(reason string) { m.egressRefusals.WithLabelValues(reason).Inc() }
func (m *Metrics) ObserveSourceSelectionFailure(reason string) {
	m.sourceSelFailures.WithLabelValues(reason).Inc()
}

// ObserveBelowFloorRequeue records n CIDs enqueued by one below-floor sweep.
func (m *Metrics) ObserveBelowFloorRequeue(n int) {
	if n > 0 {
		m.belowFloorRequeue.Add(float64(n))
	}
}
