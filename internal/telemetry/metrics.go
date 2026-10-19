package telemetry

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics are the gateway's Prometheus metrics (spec §14), served on MCPGW_METRICS_ADDR (8211).
// There are no token or cost metrics: the gateway calls no model. Every method is nil-safe.
type Metrics struct {
	reg *prometheus.Registry

	toolCalls       *prometheus.CounterVec
	overhead        prometheus.Histogram
	decisions       *prometheus.CounterVec
	replays         prometheus.Counter
	injectionFlags  *prometheus.CounterVec
	auditFlushLag   prometheus.Histogram
	policyFailures  prometheus.Counter
	auditRejections prometheus.Counter
}

// Probes supply the gauges read at scrape time.
type Probes struct {
	AuditDepth      func() int
	AuditCapacity   func() int
	PendingApproval func(ctx context.Context) (count int64, oldest time.Duration, err error)
	Upstreams       func() map[string]bool // name -> up
	PolicyLoaded    func() bool
	PolicyStale     func() bool // the last reload failed and the previous bundle is still serving
}

// NewMetrics builds the registry with the gateway metrics and the Go and process collectors.
func NewMetrics(p Probes) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{reg: reg,
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpgw_tool_calls_total",
			Help: "tools/call outcomes by tenant, upstream server, tool and audit status."}, []string{"tenant", "server", "tool", "status"}),
		overhead: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "mcpgw_overhead_seconds",
			Help:    "Gateway time per tools/call excluding the upstream call (NFR-1: p95 < 5 ms).",
			Buckets: []float64{0.0005, 0.001, 0.002, 0.003, 0.005, 0.0075, 0.01, 0.025, 0.05, 0.1, 0.25, 1}}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpgw_policy_decisions_total",
			Help: "Policy decisions for tools/call (allow, require_approval, deny)."}, []string{"decision"}),
		replays: prometheus.NewCounter(prometheus.CounterOpts{Name: "mcpgw_idempotency_replays_total",
			Help: "Write calls answered from the idempotency store without calling the upstream."}),
		injectionFlags: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcpgw_injection_flags_total",
			Help: "Upstream results whose injection score reached the threshold."}, []string{"server"}),
		auditFlushLag: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "mcpgw_audit_flush_seconds",
			Help:    "Time to persist one audit batch (SLO: p99 < 500 ms).",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}}),
		policyFailures: prometheus.NewCounter(prometheus.CounterOpts{Name: "mcpgw_policy_reload_failures_total",
			Help: "Policy bundle loads or reloads that failed (the previous bundle stays active)."}),
		auditRejections: prometheus.NewCounter(prometheus.CounterOpts{Name: "mcpgw_audit_rejections_total",
			Help: "Calls rejected with -32048 because the audit buffer was saturated (not audited themselves)."}),
	}
	reg.MustRegister(m.toolCalls, m.overhead, m.decisions, m.replays, m.injectionFlags, m.auditFlushLag, m.policyFailures,
		m.auditRejections, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if p.AuditDepth != nil {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "mcpgw_audit_buffer_depth",
			Help: "Reserved or queued audit events not yet persisted."}, func() float64 { return float64(p.AuditDepth()) }))
	}
	if p.AuditCapacity != nil {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "mcpgw_audit_buffer_capacity",
			Help: "Audit buffer size (MCPGW_AUDIT_BUFFER)."}, func() float64 { return float64(p.AuditCapacity()) }))
	}
	if p.PolicyLoaded != nil {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "mcpgw_policy_loaded",
			Help: "1 when a policy bundle is loaded; 0 means every call is denied."}, func() float64 { return b2f(p.PolicyLoaded()) }))
	}
	if p.PolicyStale != nil {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "mcpgw_policy_stale",
			Help: "1 while the last policy reload failed and the previous bundle is still serving."}, func() float64 { return b2f(p.PolicyStale()) }))
	}
	if p.PendingApproval != nil || p.Upstreams != nil {
		reg.MustRegister(&scrapeCollector{probes: p})
	}
	return m
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Handler serves the registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// Registry exposes the registry (tests).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// ToolCall counts one tools/call outcome.
func (m *Metrics) ToolCall(tenant, server, tool, status string) {
	if m != nil {
		m.toolCalls.WithLabelValues(tenant, server, tool, status).Inc()
	}
}

// Overhead observes the gateway's own time for one call.
func (m *Metrics) Overhead(d time.Duration) {
	if m != nil {
		m.overhead.Observe(d.Seconds())
	}
}

// PolicyDecision counts a decision.
func (m *Metrics) PolicyDecision(decision string) {
	if m != nil {
		m.decisions.WithLabelValues(decision).Inc()
	}
}

// Replay counts an idempotent replay.
func (m *Metrics) Replay() {
	if m != nil {
		m.replays.Inc()
	}
}

// InjectionFlag counts a flagged upstream result.
func (m *Metrics) InjectionFlag(server string) {
	if m != nil {
		m.injectionFlags.WithLabelValues(server).Inc()
	}
}

// AuditFlush observes one persisted audit batch.
func (m *Metrics) AuditFlush(d time.Duration) {
	if m != nil {
		m.auditFlushLag.Observe(d.Seconds())
	}
}

// PolicyFailure counts a failed bundle load or reload.
func (m *Metrics) PolicyFailure() {
	if m != nil {
		m.policyFailures.Inc()
	}
}

// AuditRejected counts a call refused with -32048.
func (m *Metrics) AuditRejected() {
	if m != nil {
		m.auditRejections.Inc()
	}
}

var (
	pendingDesc = prometheus.NewDesc("mcpgw_approvals_pending", "Pending, unexpired approvals.", nil, nil)
	oldestDesc  = prometheus.NewDesc("mcpgw_approvals_oldest_pending_seconds",
		"Age of the oldest pending, unexpired approval (0 when none).", nil, nil)
	upDesc = prometheus.NewDesc("mcpgw_upstream_up", "1 when the upstream answered its last catalog refresh.", []string{"upstream"}, nil)
)

// scrapeCollector reads approvals and upstream health when Prometheus scrapes.
type scrapeCollector struct{ probes Probes }

func (c *scrapeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- pendingDesc
	ch <- oldestDesc
	ch <- upDesc
}

func (c *scrapeCollector) Collect(ch chan<- prometheus.Metric) {
	if c.probes.PendingApproval != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		n, oldest, err := c.probes.PendingApproval(ctx)
		cancel()
		if err == nil {
			ch <- prometheus.MustNewConstMetric(pendingDesc, prometheus.GaugeValue, float64(n))
			ch <- prometheus.MustNewConstMetric(oldestDesc, prometheus.GaugeValue, oldest.Seconds())
		}
	}
	if c.probes.Upstreams != nil {
		for name, up := range c.probes.Upstreams() {
			ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, b2f(up), name)
		}
	}
}
