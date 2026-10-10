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

	// defaultStatementPath is the fallback guarded endpoint used when no
	// evaluator is configured (Tier-1-only mode). Configured engines override
	// it via Engine.StatementPath.
	defaultStatementPath = "/v1/statement"
)

// ──────────────────────────────────────────────────────────────────────────────
// Handler — primary HTTP handler
// ──────────────────────────────────────────────────────────────────────────────

// Handler intercepts `POST <engine.StatementPath()>`, runs Tier 1 (static
// AST) and Tier 2 (pre-flight cost) checks, and proxies approved or bypassed
// requests upstream. It depends only on the engine.Engine and parser.Dialect
// interfaces, never on a concrete engine. It also serves the `GET /healthz`
// liveness probe and (when enabled) the Prometheus metrics endpoint. Everything
// else is proxied directly, so metadata/utility endpoints and engine protocol
// calls pass through untouched.
type Handler struct {
	cfg       *config.Config
	evaluator engine.Engine
	dialect   parser.Dialect
	proxy     atomic.Pointer[httputil.ReverseProxy]
	logger    *slog.Logger
	metrics   *telemetry.Metrics
	engine    string
}

// NewHandler builds a Handler. evaluator may be nil, in which case Tier 2 is
// skipped and only Tier 1 static checks apply (fail-open). A nil logger falls
// back to slog.Default(). Telemetry is attached separately via SetMetrics.
func NewHandler(cfg *config.Config, evaluator engine.Engine, logger *slog.Logger) (*Handler, error) {
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
		dialect:   parser.For(engineName),
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

// statementPath returns the guarded endpoint for the configured engine. With
// no evaluator (Tier-1-only mode) it falls back to the Trino-style default so
// static checks still apply.
func (h *Handler) statementPath() string {
	if h.evaluator == nil {
		return defaultStatementPath
	}
	return h.evaluator.StatementPath()
}

// ServeHTTP routes requests. Health and metrics endpoints are served locally;
// `POST <statementPath>` is evaluated; all other paths (bypass endpoints such
// as /v1/info, engine protocol calls) are proxied directly.
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

	case r.Method == http.MethodPost && r.URL.Path == h.statementPath():
		h.handleStatement(w, r)
		return

	case r.Method == http.MethodGet && r.URL.Path == h.statementPath() && r.URL.Query().Get("query") != "":
		// ClickHouse accepts statements in the ?query= parameter on GET too.
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
		if h.cfg.Get().Server.OversizedBody == "reject" {
			// Comment padding past the cap is a known guard-evasion vector.
			log.Warn("statement body exceeds inspection cap; rejecting (server.oversized_body: reject)",
				slog.Int("cap_bytes", maxStatementBodyBytes), slog.Int("body_bytes", len(body)))
			h.record(telemetry.StatusBlocked)
			h.reject(r, w, ReasonStatementTooLarge,
				fmt.Sprintf("statement body of %d bytes exceeds the %d byte inspection cap (server.oversized_body: reject)", len(body), maxStatementBodyBytes))
			return
		}
		// Cannot inspect it: fail open by forwarding the full, un-truncated
		// request (already-read prefix + remaining body) so the real query is
		// never corrupted.
		log.Warn("statement body exceeds inspection cap; forwarding unguarded (fail-open)",
			slog.Int("cap_bytes", maxStatementBodyBytes),
			slog.String("query_excerpt", queryExcerpt(string(body), 200)))
		h.record(telemetry.StatusBypassed)
		h.forwardFull(w, r, body)
		return
	}
	_ = r.Body.Close()

	// The statement can arrive in the body or in the ?query= URL parameter
	// (ClickHouse accepts both; GET requests carry only the parameter).
	// Whichever holds it goes through the pipeline below; the request itself
	// is forwarded unchanged.
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	bodyStmt := strings.TrimSpace(string(body))
	if query == "" && bodyStmt == "" {
		h.record(telemetry.StatusBypassed)
		h.forward(w, r, body)
		return
	}
	if query != "" && bodyStmt != "" {
		// Parameter holds the statement, body holds accompanying data
		// (ClickHouse INSERT ... FORMAT <fmt> sends row data this way). The
		// engine does not execute the body as SQL, but blocklists still scan
		// it so nothing rides along uninspected.
		if res, err := h.dialect.Analyze(bodyStmt); err == nil {
			if reason, msg, allow := h.tier1(res); !allow {
				log.Info("query rejected by tier 1 (request body)",
					slog.String("stage", string(reason)), slog.String("statement_class", res.StatementClass.String()))
				h.auditRejection(r, bodyStmt, reason, res, nil)
				h.record(telemetry.StatusBlocked)
				h.reject(r, w, reason, msg)
				return
			}
		}
	}
	if query == "" {
		query = bodyStmt
	}

	// Clients such as DBeaver's Trino driver append a trailing semicolon, which
	// would make the shadow `EXPLAIN ...;` invalid and fail the pre-flight
	// open. The original body is still forwarded upstream unchanged.
	query = strings.TrimRight(query, "; \t\n\r")

	res, err := h.dialect.Analyze(query)
	if err != nil {
		// Fail-open: parser error forwards the original query.
		log.Error("parser analyze error; forwarding (fail-open)", slog.String("error", err.Error()))
		h.metricsParserError()
		h.record(telemetry.StatusBypassed)
		h.forward(w, r, body)
		return
	}

	// Qualify bare table references with the request's catalog/schema context
	// so required-filter scoping cannot be evaded by omitting the qualifier
	// (engines resolve bare names against the session catalog/schema or the
	// default database).
	h.qualifyBareTables(res, r)

	// Executing EXPLAIN forms (EXPLAIN ANALYZE / TYPE EXECUTE) run their inner
	// statement: they must not bypass Tier 1, and Tier 2 must pre-flight the
	// inner query rather than the wrapper.
	isExecExplain := false
	preflightQuery := query
	if res.StatementClass == parser.StatementExplain {
		if inner, ok := parser.SplitExecutingExplain(query); ok {
			isExecExplain = true
			preflightQuery = inner
		}
	}

	// Bypass statements (SHOW, DESCRIBE, EXPLAIN, USE, SET) skip both tiers.
	if parser.IsBypass(res.StatementClass) && !isExecExplain {
		h.record(telemetry.StatusBypassed)
		h.forward(w, r, body)
		return
	}

	// Tier 1: static AST checks — any violation is an immediate hard reject.
	if reason, msg, allow := h.tier1(res); !allow {
		log.Info("query rejected by tier 1",
			slog.String("stage", string(reason)), slog.String("statement_class", res.StatementClass.String()))
		h.auditRejection(r, query, reason, res, nil)
		h.record(telemetry.StatusBlocked)
		h.reject(r, w, reason, msg)
		return
	}

	// Tier 2: pre-flight cost evaluation for read queries with cost limits.
	// Executing EXPLAIN forms are pre-flighted on their inner statement so
	// cost limits apply to the query that actually runs.
	if h.shouldPreflight(res.StatementClass) || isExecExplain {
		allowed, reason, cost, err := h.tier2(r.Context(), r.Header, preflightQuery)
		switch {
		case err != nil:
			if failOpen := h.cfg.Get().Preflight.FailOpen; failOpen != nil && !*failOpen {
				// Fail-closed: the pre-flight could not complete, so the
				// cost limits cannot be enforced — reject instead of forward.
				log.Info("query rejected by tier 2 (fail-closed)", slog.String("stage", string(ReasonPreflightFailed)))
				h.auditRejection(r, query, ReasonPreflightFailed, res, &cost)
				h.record(telemetry.StatusBlocked)
				h.reject(r, w, ReasonPreflightFailed,
					"pre-flight evaluation could not complete and preflight.fail_open is false")
				return
			}
			// Fail-open: evaluation could not complete; forward the query.
		case !allowed:
			// Definitive limit breach → reject. (Blocked bytes were attributed
			// to the telemetry counter inside tier2.)
			log.Info("query rejected by tier 2",
				slog.String("stage", string(reason)))
			h.auditRejection(r, query, ReasonCostLimitBreach, res, &cost)
			h.record(telemetry.StatusBlocked)
			h.reject(r, w, ReasonCostLimitBreach, reason)
			return
		case h.cfg.Get().Preflight.RequireEstimates && len(res.Tables) > 0 && len(cost.TableScans) == 0:
			// The statement references tables but the plan carries no per-table
			// estimates (views, table functions, missing statistics): the
			// table-scoped limits are inert. Fail closed when configured.
			log.Info("query rejected by tier 2 (no table estimates)", slog.String("stage", string(ReasonPreflightNoEstimates)))
			h.auditRejection(r, query, ReasonPreflightNoEstimates, res, &cost)
			h.record(telemetry.StatusBlocked)
			h.reject(r, w, ReasonPreflightNoEstimates,
				"plan carries no per-table estimates for referenced tables and preflight.require_estimates is true")
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

	// Function blocklist (Feature B): any blocked function appearing in the
	// projections or WHERE/ON clauses (including CTEs and predicate
	// subqueries) is rejected.
	for _, fn := range res.Functions {
		for _, blocked := range p.Rules.FunctionBlocklist {
			if strings.EqualFold(fn, blocked) {
				return ReasonFunctionBlocklist, fmt.Sprintf("function %q is blocked by policy", fn), false
			}
		}
	}

	// Table-function blocklist: a table-valued function reads rows without
	// naming a physical table (merge('db','regex'), remote('host', db, table)),
	// so table_blocklist cannot see through it.
	for _, fn := range res.TableFunctions {
		for _, blocked := range p.Rules.TableFunctionBlocklist {
			if strings.EqualFold(fn, blocked) {
				return ReasonTableFunctionBlocklist, fmt.Sprintf("table function %q is blocked by policy", fn), false
			}
		}
	}

	// SELECT * restriction (Feature C): projection-level stars over listed
	// tables are rejected; explicit column lists are fine. COUNT(*) is not a
	// projection star and is never flagged.
	if res.SelectAll {
		for _, bad := range p.Rules.SelectStarBlockedTables {
			for _, table := range res.Tables {
				if tableBlocklisted(table, bad) {
					return ReasonSelectStarBlocked, fmt.Sprintf("SELECT * on table %q is blocked (specify explicit columns)", table), false
				}
			}
		}
	}

	// Required filters (Feature E): entries match exactly one schema.table
	// (plus catalog when set) — no suffix/bare-name breadth. Column filtering
	// must be attributed to that specific table; mode is any-of or all-of.
	for _, rf := range p.Rules.RequiredFilters {
		for _, table := range res.Tables {
			if !requiredFilterMatches(table, rf) {
				continue
			}
			missing := missingRequiredColumns(res, table, rf)
			if len(missing) > 0 {
				return ReasonRequiredFilter, fmt.Sprintf("query on table %q must filter on column(s) %s (required filter, mode %s)",
					table, strings.Join(missing, ", "), requiredFilterMode(rf)), false
			}
		}
	}

	return ReasonTableBlocklist, "", true
}

// qualifyBareTables prefixes bare (single-component) table references with
// the request's catalog/schema context so required-filter scoping cannot be
// evaded by omitting the qualifier. Trino resolves bare names against the
// X-Trino-Catalog / X-Trino-Schema session headers; ClickHouse against the
// X-ClickHouse-Database header, the ?database= URL parameter, or "default".
func (h *Handler) qualifyBareTables(res *parser.AnalysisResult, r *http.Request) {
	var catalog, schema string
	switch h.engine {
	case "trino":
		catalog = r.Header.Get("X-Trino-Catalog")
		schema = r.Header.Get("X-Trino-Schema")
	case "clickhouse":
		schema = r.Header.Get("X-ClickHouse-Database")
		if schema == "" {
			schema = r.URL.Query().Get("database")
		}
		if schema == "" {
			schema = "default"
		}
	default:
		return
	}
	if catalog == "" && schema == "" {
		return
	}
	res.Tables = parser.QualifyBareTables(res.Tables, catalog, schema)
}

// queryExcerpt returns the first max runes of a query with newlines collapsed,
// for fail-open log lines where the full statement would be unwieldy.
func queryExcerpt(query string, max int) string {
	flat := strings.Join(strings.Fields(query), " ")
	if len(flat) <= max {
		return flat
	}
	return flat[:max] + "…"
}

// requiredFilterMatches reports whether a referenced table matches a required
// filter entry. Unlike tableMatchesScope, matching is strict: schema.table
// must both be present in the reference (catalog too, when set) — no
// suffix/bare-table breadth. Case-insensitive (Trino unquoted identifiers).
func requiredFilterMatches(table string, rf config.RequiredFilter) bool {
	parts := strings.Split(table, ".")
	if len(parts) < 2 {
		return false
	}
	if !strings.EqualFold(parts[len(parts)-1], rf.Table) {
		return false
	}
	if !strings.EqualFold(parts[len(parts)-2], rf.Schema) {
		return false
	}
	if rf.Catalog != "" {
		if len(parts) < 3 || !strings.EqualFold(parts[len(parts)-3], rf.Catalog) {
			return false
		}
	}
	return true
}

// requiredFilterMode returns the effective mode of an entry ("any-of" or
// "all-of"); an empty configured mode defaults to all-of.
func requiredFilterMode(rf config.RequiredFilter) string {
	if rf.Mode == "any-of" {
		return "any-of"
	}
	return "all-of"
}

// missingRequiredColumns returns the required columns that are NOT filtered
// on for the given table. For all-of mode (the default) that is every absent
// column; for any-of mode it is all columns when none are present (the query
// is then rejected), or empty when at least one is present (allowed).
func missingRequiredColumns(res *parser.AnalysisResult, table string, rf config.RequiredFilter) []string {
	var missing []string
	for _, col := range rf.Columns {
		if !tableHasFilter(res, table, col) {
			missing = append(missing, col)
		}
	}
	if requiredFilterMode(rf) == "any-of" && len(missing) < len(rf.Columns) {
		return nil // at least one required column is filtered → satisfied
	}
	return missing
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

// tier2 runs the pre-flight cost evaluation. It returns (allowed, reason,
// cost, err). The contract is explicit:
//   - err != nil  → evaluation could not complete → caller fails open (forward).
//   - err == nil && !allowed → definitive limit breach → caller rejects.
//   - err == nil && allowed  → within limits → caller forwards.
//
// cost is populated whenever err == nil (including rejections) so the audit
// snapshot can include the estimates that triggered the block.
func (h *Handler) tier2(ctx context.Context, headers http.Header, query string) (bool, string, engine.CostResult, error) {
	start := time.Now()
	cost, err := h.evaluator.Evaluate(ctx, query, headers)
	h.observePreflight(time.Since(start))

	if err != nil {
		if errors.Is(err, engine.ErrPreflightConcurrency) {
			h.metricsPreflightRejected()
		}
		// Fail-open: log and forward. The engine logs the error with the
		// request ID from ctx; record the stage here too, with a query excerpt
		// so fail-open forwards are attributable in the logs.
		h.logger.Error("pre-flight evaluation error (fail-open)",
			slog.String("request_id", ctxlog.FromContext(ctx)),
			slog.String("error", err.Error()),
			slog.String("query_excerpt", queryExcerpt(query, 200)))
		return false, "", cost, err
	}

	if !cost.Allowed {
		// Blocked: attribute the estimated bytes to the blocked-byte counter.
		h.metricsBlockedBytes(cost.EstimatedScanBytes)
		return false, cost.Reason, cost, nil
	}
	return true, "", cost, nil
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
// Audit logging (Feature D) — rejections only, strictly opt-in
// ──────────────────────────────────────────────────────────────────────────────

// auditRejection emits one structured audit line for a rejected query, only
// when audit.log_rejections is enabled (read live from config, so hot-reloads
// apply). Identity fields (X-Trino-User, X-Trino-Client-Tags) appear ONLY in
// these opt-in audit records — never in other logs (see TestNoCredentialLogging).
// Authorization values are never logged anywhere, audit lines included.
// cost is non-nil only for Tier-2 rejections; the plan snapshot is included
// only when audit.snapshot_plan is enabled.
func (h *Handler) auditRejection(r *http.Request, query string, reason RejectionReason, res *parser.AnalysisResult, cost *engine.CostResult) {
	p := h.cfg.Get()
	if !p.Audit.LogRejections {
		return
	}

	attrs := []slog.Attr{
		slog.String("query", query),
		slog.String("reason", string(reason)),
		slog.Any("tables", res.Tables),
		slog.Any("where_columns", res.WhereColumns),
		slog.String("user", r.Header.Get("X-Trino-User")),
		slog.String("client_tags", r.Header.Get("X-Trino-Client-Tags")),
		slog.String("request_id", ctxlog.FromContext(r.Context())),
	}

	if p.Audit.SnapshotPlan && cost != nil {
		scans := make([]map[string]any, 0, len(cost.TableScans))
		for _, s := range cost.TableScans {
			scans = append(scans, map[string]any{
				"table":      s.Table,
				"scan_bytes": s.ScanBytes,
				"rows":       s.Rows,
			})
		}
		attrs = append(attrs, slog.Any("snapshot", map[string]any{
			"estimated_bytes": cost.EstimatedScanBytes,
			"estimated_rows":  cost.EstimatedRows,
			"estimated_cpu":   cost.EstimatedCPUCost,
			"table_scans":     scans,
		}))
	}

	h.logger.LogAttrs(r.Context(), slog.LevelInfo, "audit_rejection", attrs...)
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
