package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync/atomic"
	"time"

	"query-guard/internal/config"
	"query-guard/internal/ctxlog"
	"query-guard/internal/engine"
	"query-guard/internal/parser"
	"query-guard/internal/telemetry"
)

const (
	// maxStatementBodyBytes caps how much of a statement body we buffer for
	// inspection. Larger bodies would only be truncated and could produce a
	// partial parse; guarded statements are still forwarded upstream.
	maxStatementBodyBytes = 8 << 20 // 8 MiB

	// healthzPath is the liveness endpoint.
	healthzPath = "/healthz"

	// readyzPath is the readiness endpoint (checks upstream reachability).
	readyzPath = "/readyz"

	// defaultMetricsPath is used when TelemetryConfig.Path is empty.
	defaultMetricsPath = "/metrics"
)

// ──────────────────────────────────────────────────────────────────────────────
// Handler — primary HTTP handler
// ──────────────────────────────────────────────────────────────────────────────

// Handler intercepts `POST /v1/statement`, runs Tier 1 (static AST) and Tier 2
// (pre-flight cost) checks, and proxies approved or bypassed requests upstream.
// It also serves the `GET /healthz` liveness probe and (when enabled) the
// Prometheus metrics endpoint. Everything else is proxied directly, so
// metadata/utility endpoints and engine protocol calls pass through untouched.
type Handler struct {
	cfg       *config.Config
	evaluator engine.CostEvaluator
	proxy     atomic.Pointer[httputil.ReverseProxy]
	logger    *slog.Logger
	metrics   *telemetry.Metrics
	engine    string
}

// NewHandler builds a Handler. evaluator may be nil, in which case Tier 2 is
// skipped and only Tier 1 static checks apply (fail-open). A nil logger falls
// back to slog.Default(). Telemetry is attached separately via SetMetrics.
func NewHandler(cfg *config.Config, evaluator engine.CostEvaluator, logger *slog.Logger) (*Handler, error) {
	if logger == nil {
		logger = slog.Default()
	}

	p := cfg.Get()
	rp, err := newReverseProxy(p.Upstream.URL)
	if err != nil {
		return nil, err
	}

	engineName := "trino"
	if evaluator != nil {
		engineName = evaluator.Engine()
	}

	h := &Handler{
		cfg:       cfg,
		evaluator: evaluator,
		logger:    logger,
		engine:    engineName,
	}
	h.proxy.Store(rp)

	return h, nil
}

// currentProxy returns the active reverse proxy. Concurrent UpdateUpstream
// calls swap this atomically, so requests always use a consistent target.
func (h *Handler) currentProxy() *httputil.ReverseProxy {
	return h.proxy.Load()
}

// UpdateUpstream rebuilds the reverse proxy to point at a new upstream URL.
// It is safe to call at runtime (e.g. from the config hot-reload callback):
// in-flight and future requests atomically switch to the new target.
func (h *Handler) UpdateUpstream(url string) error {
	rp, err := newReverseProxy(url)
	if err != nil {
		return err
	}
	h.proxy.Store(rp)
	return nil
}

// SetMetrics attaches a telemetry sink. Passing nil leaves telemetry disabled.
func (h *Handler) SetMetrics(m *telemetry.Metrics) {
	h.metrics = m
}

// ServeHTTP routes requests. Health and metrics endpoints are served locally;
// `POST /v1/statement` is evaluated; all other paths (bypass endpoints such as
// /v1/info, engine protocol calls) are proxied directly.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == healthzPath:
		h.handleHealthz(w, r)
		return

	case r.URL.Path == readyzPath:
		h.handleReadyz(w, r)
		return

	case h.metrics != nil && r.Method == http.MethodGet && r.URL.Path == h.metricsPath():
		h.metrics.Handler().ServeHTTP(w, r)
		return

	case r.Method == http.MethodPost && r.URL.Path == statementPath:
		h.handleStatement(w, r)
		return

	default:
		// Direct passthrough for all non-statement endpoints.
		h.currentProxy().ServeHTTP(w, r)
	}
}

// handleHealthz serves a liveness probe. A 200 response indicates the process
// is up and able to serve requests.
func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	_ = r
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// handleReadyz serves a readiness probe. It checks that the upstream is
// reachable and healthy; returns 503 when it is not, so orchestrators can avoid
// routing traffic to a proxy whose upstream is down.
func (h *Handler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	upstream := strings.TrimRight(h.cfg.Get().Upstream.URL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream+"/v1/info", nil)
	if err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "not ready: upstream unreachable", http.StatusServiceUnavailable)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "not ready: upstream unhealthy", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// metricsPath returns the configured metrics endpoint path, or "" when
// telemetry is disabled.
func (h *Handler) metricsPath() string {
	p := h.cfg.Get()
	if !p.Telemetry.Enabled {
		return ""
	}
	if p.Telemetry.Path == "" {
		return defaultMetricsPath
	}
	return p.Telemetry.Path
}

// handleStatement applies the two-tier evaluation pipeline to an incoming
// query. The full body is buffered once (bounded by maxStatementBodyBytes) so
// it can be parsed and then replayed upstream without losing the payload.
func (h *Handler) handleStatement(w http.ResponseWriter, r *http.Request) {
	// Establish request correlation: honor an inbound X-Request-ID or mint a
	// fresh one, and carry it through the context so the engine's log lines
	// and this handler's logs (and the rejection body) all share one key.
	reqID := r.Header.Get(ctxlog.HeaderName)
	if reqID == "" {
		reqID = ctxlog.NewID()
	}
	r = r.WithContext(ctxlog.WithRequestID(r.Context(), reqID))
	log := h.logger.With("request_id", reqID)

	// Read up to max+1 bytes so we can detect a body larger than the cap.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxStatementBodyBytes+1))
	if err != nil {
		_ = r.Body.Close()
		// Fail-open: cannot inspect, so forward the query untouched.
		log.Error("failed to read statement body; forwarding (fail-open)", slog.String("error", err.Error()))
		h.record(telemetry.StatusBypassed)
		h.forward(w, r, body)
		return
	}

	if len(body) > maxStatementBodyBytes {
		// Oversized body: we cannot safely inspect it. Fail open by forwarding
		// the full, un-truncated request (already-read prefix + remaining body)
		// so the real query is never corrupted.
		log.Warn("statement body exceeds inspection cap; forwarding unguarded (fail-open)", slog.Int("cap_bytes", maxStatementBodyBytes))
		h.record(telemetry.StatusBypassed)
		h.forwardFull(w, r, body)
		return
	}
	_ = r.Body.Close()

	query := strings.TrimSpace(string(body))
	if query == "" {
		// Nothing to evaluate; forward the empty statement to the engine.
		h.record(telemetry.StatusBypassed)
		h.forward(w, r, body)
		return
	}

	// Normalise the statement for analysis and pre-flight. Clients such as
	// DBeaver's Trino driver append a trailing semicolon, which would make the
	// shadow `EXPLAIN ...;` invalid and cause the pre-flight to fail open. Strip
	// trailing semicolons and whitespace so the guard actually evaluates the
	// query. The original body is still forwarded upstream unchanged.
	query = strings.TrimRight(query, "; \t\n\r")

	res, err := parser.Analyze(query)
	if err != nil {
		// Fail-open: parser error forwards the original query.
		log.Error("parser analyze error; forwarding (fail-open)", slog.String("error", err.Error()))
		h.metricsParserError()
		h.record(telemetry.StatusBypassed)
		h.forward(w, r, body)
		return
	}

	// Bypass statements (SHOW, DESCRIBE, EXPLAIN, USE, SET) skip both tiers.
	if parser.IsBypass(res.StatementClass) {
		h.record(telemetry.StatusBypassed)
		h.forward(w, r, body)
		return
	}

	// Tier 1: static AST checks — any violation is an immediate hard reject.
	if reason, msg, allow := h.tier1(res); !allow {
		log.Info("query rejected by tier 1",
			slog.String("stage", string(reason)), slog.String("statement_class", res.StatementClass.String()))
		h.record(telemetry.StatusBlocked)
		h.reject(r, w, reason, msg)
		return
	}

	// Tier 2: pre-flight cost evaluation for read queries with cost limits.
	if h.shouldPreflight(res.StatementClass) {
		allowed, reason, err := h.tier2(r.Context(), r.Header, query)
		if err != nil {
			// Fail-open: evaluation could not complete; forward the query.
		} else if !allowed {
			// Definitive limit breach → reject. (Blocked bytes were attributed
			// to the telemetry counter inside tier2.)
			log.Info("query rejected by tier 2",
				slog.String("stage", string(reason)))
			h.record(telemetry.StatusBlocked)
			h.reject(r, w, ReasonCostLimitBreach, reason)
			return
		}
	}

	h.record(telemetry.StatusAllowed)
	h.forward(w, r, body)
}

// ──────────────────────────────────────────────────────────────────────────────
// Tier 1 — static AST checks
// ──────────────────────────────────────────────────────────────────────────────

// tier1 applies statement blocklist, table blocklist, and required-filter
// checks. It returns (reason, message, allowed). When rejected the reason
// categorises the violation and message describes it.
func (h *Handler) tier1(res *parser.AnalysisResult) (RejectionReason, string, bool) {
	p := h.cfg.Get()

	// Statement blocklist.
	for _, blocked := range p.Rules.StatementBlocklist {
		if strings.EqualFold(blocked, res.StatementClass.String()) {
			return ReasonStatementBlocklist, fmt.Sprintf("statement type %s is blocked by policy", res.StatementClass), false
		}
	}

	// Table blocklist.
	for _, bad := range p.Rules.TableBlocklist {
		for _, table := range res.Tables {
			if tableBlocklisted(table, bad) {
				return ReasonTableBlocklist, fmt.Sprintf("table %q is blocked by policy (matched %q)", table, bad), false
			}
		}
	}

	// Required filters: any table with a configured filter must reference the
	// required column in its WHERE/ON clause, on that specific table.
	for _, rf := range p.Rules.RequiredFilters {
		for _, table := range res.Tables {
			if !tableMatchesScope(table, rf.Catalog, rf.Schema, rf.Table) {
				continue
			}
			if !tableHasFilter(res, table, rf.Column) {
				return ReasonRequiredFilter, fmt.Sprintf("query on table %q must filter on column %q (required filter)", table, rf.Column), false
			}
		}
	}

	return ReasonTableBlocklist, "", true
}

// tableBlocklisted reports whether a table name matches a blocklist entry.
// A dot-free entry matches the trailing table component; a qualified entry
// must match the full name. Matching is case-insensitive (Trino unquoted
// identifiers are case-insensitive).
func tableBlocklisted(table, entry string) bool {
	if strings.EqualFold(table, entry) {
		return true
	}
	if !strings.Contains(entry, ".") {
		parts := strings.Split(table, ".")
		return strings.EqualFold(parts[len(parts)-1], entry)
	}
	return false
}

// tableMatchesScope reports whether a scan table matches a catalog/schema/table
// scope, matching on the rightmost components (suffix semantics).
func tableMatchesScope(scanTable, catalog, schema, table string) bool {
	parts := strings.Split(scanTable, ".")
	if table != "" && !strings.EqualFold(parts[len(parts)-1], table) {
		return false
	}
	if schema != "" {
		if len(parts) < 2 || !strings.EqualFold(parts[len(parts)-2], schema) {
			return false
		}
	}
	if catalog != "" {
		if len(parts) < 3 || !strings.EqualFold(parts[len(parts)-3], catalog) {
			return false
		}
	}
	return true
}

// tableHasFilter reports whether the guarded table is actually filtered on the
// required column. It uses qualified column references so a predicate on a
// *different* table (e.g. a join condition) cannot satisfy the requirement.
// An unqualified reference is assumed to target the guarded table (the common
// `WHERE col = ...` form).
func tableHasFilter(res *parser.AnalysisResult, table, column string) bool {
	tableLast := lastComponent(table)
	for _, ref := range res.WhereRefs {
		if !strings.EqualFold(ref.Column, column) {
			continue
		}
		if ref.Table == "" {
			return true
		}
		resolved := ref.Table
		if alias, ok := res.TableAliases[ref.Table]; ok {
			resolved = alias
		}
		if strings.EqualFold(resolved, tableLast) || strings.EqualFold(resolved, table) {
			return true
		}
	}
	return false
}

// lastComponent returns the rightmost dot-separated component of a name.
func lastComponent(name string) string {
	parts := strings.Split(name, ".")
	return parts[len(parts)-1]
}

// ──────────────────────────────────────────────────────────────────────────────
// Tier 2 — pre-flight cost evaluation
// ──────────────────────────────────────────────────────────────────────────────

// shouldPreflight reports whether Tier 2 should run. Only SELECT queries run
// when cost limits are configured; bypass and mutating statements never do.
func (h *Handler) shouldPreflight(class parser.StatementClass) bool {
	if class != parser.StatementSelect {
		return false
	}
	p := h.cfg.Get()
	return len(p.Rules.CostLimits) > 0 && h.evaluator != nil
}

// tier2 runs the pre-flight cost evaluation. It returns (allowed, reason, err).
// The contract is explicit:
//   - err != nil  → evaluation could not complete → caller fails open (forward).
//   - err == nil && !allowed → definitive limit breach → caller rejects.
//   - err == nil && allowed  → within limits → caller forwards.
func (h *Handler) tier2(ctx context.Context, headers http.Header, query string) (bool, string, error) {
	start := time.Now()
	res, err := h.evaluator.Evaluate(ctx, query, headers)
	h.observePreflight(time.Since(start))

	if err != nil {
		if errors.Is(err, engine.ErrPreflightConcurrency) {
			h.metricsPreflightRejected()
		}
		// Fail-open: log and forward. The engine logs the error with the
		// request ID from ctx; record the stage here too.
		h.logger.Error("pre-flight evaluation error (fail-open)",
			slog.String("request_id", ctxlog.FromContext(ctx)),
			slog.String("error", err.Error()))
		return false, "", err
	}

	if !res.Allowed {
		// Blocked: attribute the estimated bytes to the blocked-byte counter.
		h.metricsBlockedBytes(res.EstimatedScanBytes)
		return false, res.Reason, nil
	}
	return true, "", nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Forwarding & rejection
// ──────────────────────────────────────────────────────────────────────────────

// forward restores the fully-read body onto the request and proxies it
// upstream.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	h.currentProxy().ServeHTTP(w, r)
}

// forwardFull replays an already-read body prefix followed by the remaining
// request body, so an oversized statement is forwarded un-truncated (fail-open).
func (h *Handler) forwardFull(w http.ResponseWriter, r *http.Request, prefix []byte) {
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), r.Body))
	r.ContentLength = -1 // unknown; use chunked transfer
	h.currentProxy().ServeHTTP(w, r)
}

// reject writes a standard JSON 400 rejection, correlated to the request ID.
func (h *Handler) reject(r *http.Request, w http.ResponseWriter, reason RejectionReason, message string) {
	WriteRejection(w, http.StatusBadRequest, reason, message, ctxlog.FromContext(r.Context()))
}

// ──────────────────────────────────────────────────────────────────────────────
// Telemetry helpers (nil-safe)
// ──────────────────────────────────────────────────────────────────────────────

func (h *Handler) record(status telemetry.QueryStatus) {
	if h.metrics != nil {
		h.metrics.RecordQuery(status, h.engine)
	}
}

func (h *Handler) metricsParserError() {
	if h.metrics != nil {
		h.metrics.RecordParserError()
	}
}

func (h *Handler) observePreflight(d time.Duration) {
	if h.metrics != nil {
		h.metrics.ObservePreflight(d)
	}
}

func (h *Handler) metricsBlockedBytes(bytes int64) {
	if h.metrics != nil {
		h.metrics.RecordBlockedBytes(bytes)
	}
}

func (h *Handler) metricsPreflightRejected() {
	if h.metrics != nil {
		h.metrics.RecordPreflightRejected()
	}
}
