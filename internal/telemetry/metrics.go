package telemetry

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ──────────────────────────────────────────────────────────────────────────────
// Query status labels
// ──────────────────────────────────────────────────────────────────────────────

// QueryStatus labels the outcome of a client query lifecycle.
type QueryStatus string

const (
	// StatusAllowed records queries that passed all checks and were forwarded.
	StatusAllowed QueryStatus = "allowed"
	// StatusBlocked records queries rejected by Tier 1 or Tier 2.
	StatusBlocked QueryStatus = "blocked"
	// StatusBypassed records queries forwarded without enforcement (bypass
	// statements, bypass endpoints, and fail-open paths).
	StatusBypassed QueryStatus = "bypassed"
)

// defaultEngine is the engine label used when no evaluator provides one.
const defaultEngine = "trino"

// ──────────────────────────────────────────────────────────────────────────────
// Metrics — Prometheus registry and collectors
// ──────────────────────────────────────────────────────────────────────────────

// Metrics owns the Prometheus collectors for query-guard. It may be created
// with any registry so callers can share one registry across components. All
// recording methods are safe to call on a nil *Metrics (no-op), so the proxy
// handler can hold an optional pointer without nil-checking every call site.
type Metrics struct {
	registry *prometheus.Registry

	queriesTotal      *prometheus.CounterVec
	blockedBytesTotal prometheus.Counter
	preflightDuration prometheus.Histogram
	preflightRejected prometheus.Counter
	parserErrorsTotal prometheus.Counter
}

// NewMetrics builds a Metrics bound to the given registry. If registry is nil,
// a fresh default registry is created.
func NewMetrics(registry *prometheus.Registry) *Metrics {
	if registry == nil {
		registry = prometheus.NewRegistry()
	}

	m := &Metrics{registry: registry}

	m.queriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "queryguard",
		Name:      "queries_total",
		Help:      "Total number of client queries handled, tagged by outcome and engine.",
	}, []string{"status", "engine"})

	m.blockedBytesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "queryguard",
		Name:      "blocked_bytes_total",
		Help:      "Total estimated scan bytes blocked by cost-limit enforcement.",
	})

	m.preflightDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "queryguard",
		Name:      "preflight_duration_seconds",
		Help:      "Duration of Tier 2 pre-flight cost evaluations.",
		Buckets:   prometheus.DefBuckets,
	})

	m.parserErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "queryguard",
		Name:      "parser_errors_total",
		Help:      "Total number of SQL parser failures (fail-open events).",
	})

	m.preflightRejected = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "queryguard",
		Name:      "preflight_rejected_total",
		Help:      "Total number of pre-flight evaluations rejected because the concurrency gate was saturated (fail-open).",
	})

	registry.MustRegister(
		m.queriesTotal,
		m.blockedBytesTotal,
		m.preflightDuration,
		m.preflightRejected,
		m.parserErrorsTotal,
	)

	return m
}

// RecordQuery increments the per-status/engine query counter.
func (m *Metrics) RecordQuery(status QueryStatus, engine string) {
	if m == nil {
		return
	}
	if engine == "" {
		engine = defaultEngine
	}
	m.queriesTotal.WithLabelValues(string(status), engine).Inc()
}

// RecordBlockedBytes accumulates estimated scan bytes blocked by cost limits.
func (m *Metrics) RecordBlockedBytes(bytes int64) {
	if m == nil || bytes <= 0 {
		return
	}
	m.blockedBytesTotal.Add(float64(bytes))
}

// ObservePreflight records a single pre-flight evaluation duration.
func (m *Metrics) ObservePreflight(d time.Duration) {
	if m == nil {
		return
	}
	m.preflightDuration.Observe(d.Seconds())
}

// RecordParserError increments the parser error counter (fail-open events).
func (m *Metrics) RecordParserError() {
	if m == nil {
		return
	}
	m.parserErrorsTotal.Inc()
}

// RecordPreflightRejected increments the counter for pre-flight evaluations
// rejected by the concurrency gate (fail-open events).
func (m *Metrics) RecordPreflightRejected() {
	if m == nil {
		return
	}
	m.preflightRejected.Inc()
}

// Handler returns an http.Handler that serves Prometheus metrics in the
// standard text exposition format for this registry.
func (m *Metrics) Handler() http.Handler {
	if m == nil || m.registry == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
