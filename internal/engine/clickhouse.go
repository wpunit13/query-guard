package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"query-guard/internal/config"
	"query-guard/internal/telemetry"
)

// ──────────────────────────────────────────────────────────────────────────────
// ClickHouseEvaluator — Tier 2 via EXPLAIN ESTIMATE
// ──────────────────────────────────────────────────────────────────────────────

const (
	clickhouseEngineName = "clickhouse"
	// explainEstimatePrefix wraps the incoming query in a shadow row estimate.
	explainEstimatePrefix = "EXPLAIN ESTIMATE "
	// explainEstimateSuffix requests a machine-parseable response.
	explainEstimateSuffix = " FORMAT JSON"
)

// ClickHouseEvaluator implements Engine for ClickHouse clusters. It issues a
// shadow `EXPLAIN ESTIMATE <query> FORMAT JSON` request to the upstream
// server's HTTP endpoint (POST /, port 8123 by convention), mirroring all
// client headers 1:1, and parses the per-table row estimates.
//
// EXPLAIN ESTIMATE carries no scan-byte or CPU estimate, so those dimensions
// stay 0 and the shared evaluateLimits enforces rows only (0 = disabled).
type ClickHouseEvaluator struct {
	cfg         *config.Config
	upstreamURL string
	client      *http.Client
	timeout     time.Duration
	sem         chan struct{}
	logger      *slog.Logger
	metrics     *telemetry.Metrics
}

// Register the ClickHouse engine so engine.New resolves upstream.engine:
// clickhouse.
func init() {
	Register(clickhouseEngineName, func(cfg *config.Config, client *http.Client) (Engine, error) {
		return NewClickHouseEvaluator(cfg, client), nil
	})
}

// NewClickHouseEvaluator builds a ClickHouseEvaluator from the shared,
// hot-reloadable config. A nil client yields a default client. The upstream
// URL and timeout are read from cfg on every Evaluate so policy hot-reloads
// take effect immediately. The pre-flight concurrency gate is sized from
// preflight.max_concurrent.
func NewClickHouseEvaluator(cfg *config.Config, client *http.Client) *ClickHouseEvaluator {
	p := cfg.Get()

	timeout := p.Preflight.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	maxConcurrent := p.Preflight.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 5
	}

	if client == nil {
		// No client-level Timeout: the per-request context (which hot-reloads)
		// is the single source of truth for the pre-flight deadline.
		client = &http.Client{}
	}

	return &ClickHouseEvaluator{
		cfg:         cfg,
		upstreamURL: strings.TrimRight(p.Upstream.URL, "/"),
		client:      client,
		timeout:     timeout,
		sem:         make(chan struct{}, maxConcurrent),
		logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
}

// Engine returns the engine name for telemetry labels.
func (e *ClickHouseEvaluator) Engine() string {
	return clickhouseEngineName
}

// StatementPath returns the guarded ClickHouse HTTP endpoint. ClickHouse
// accepts queries on the server root via POST.
func (e *ClickHouseEvaluator) StatementPath() string {
	return "/"
}

// SetMetrics attaches a telemetry sink. Passing nil leaves telemetry
// disabled. All recording methods are nil-safe.
func (e *ClickHouseEvaluator) SetMetrics(m *telemetry.Metrics) {
	e.metrics = m
}

// Evaluate issues a shadow EXPLAIN ESTIMATE request and evaluates the
// estimated row counts against configured limits.
//
// Fail-open: any error (timeout, upstream failure, unparseable response) is
// returned so the caller can forward the original query.
func (e *ClickHouseEvaluator) Evaluate(ctx context.Context, query string, headers http.Header) (CostResult, error) {
	result := CostResult{Engine: clickhouseEngineName}

	// Read the upstream and timeout from the live config so hot-reloads apply.
	upstream := e.currentUpstream()
	timeout := e.timeout
	if e.cfg != nil {
		if t := e.cfg.Get().Preflight.Timeout; t > 0 {
			timeout = t
		}
	}

	evalCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Fail open when the gate is saturated instead of queueing unboundedly
	// against the upstream.
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-evalCtx.Done():
		return result, fmt.Errorf("%w: %v", ErrPreflightConcurrency, evalCtx.Err())
	}

	shadow := explainEstimatePrefix + stripTrailingFormatClause(query) + explainEstimateSuffix
	body, err := e.postQuery(evalCtx, upstream+"/", shadow, headers)
	if err != nil {
		return result, fmt.Errorf("engine/clickhouse: preflight statement: %w", err)
	}

	scans, rows, err := parseExplainEstimate(body)
	if err != nil {
		return result, fmt.Errorf("engine/clickhouse: parse estimate: %w", err)
	}

	result.EstimatedRows = rows
	result.TableScans = scans

	// No scan-byte or CPU estimate exists in EXPLAIN ESTIMATE: leave both at
	// 0 so evaluateLimits skips those limits (rows-only enforcement).
	result.Allowed, result.Reason = evaluateLimits(ctx, e.cfg, e.logger, e.metrics, 0, rows, scans, 0)
	return result, nil
}

// currentUpstream returns the up-to-date upstream base URL from the live
// config, so an upstream change is picked up without a restart.
func (e *ClickHouseEvaluator) currentUpstream() string {
	if e.cfg == nil {
		return e.upstreamURL
	}
	return strings.TrimRight(e.cfg.Get().Upstream.URL, "/")
}

// stripTrailingFormatClause removes a terminal `FORMAT <identifier>` clause
// (optionally followed by a SETTINGS clause) before the shadow EXPLAIN
// ESTIMATE is built. The shadow appends its own ` FORMAT JSON`; a query that
// already ends with FORMAT would produce `... FORMAT JSON FORMAT JSON`, a
// syntax error — and a pre-flight failure fails open, so the suffix must not
// collide. The original query is still forwarded upstream unchanged; only the
// shadow copy is rewritten.
func stripTrailingFormatClause(query string) string {
	fields := strings.Fields(strings.TrimSpace(query))
	for i := len(fields) - 2; i >= 0; i-- {
		if !strings.EqualFold(fields[i], "FORMAT") {
			continue
		}
		// Must be followed by an output-format identifier.
		if i+1 >= len(fields) || !isFormatName(fields[i+1]) {
			return query
		}
		rest := fields[i+2:]
		// The FORMAT pair must be terminal, or followed only by a SETTINGS
		// clause (ClickHouse grammar: query [FORMAT fmt] [SETTINGS ...]).
		if len(rest) == 0 || strings.EqualFold(rest[0], "SETTINGS") {
			out := append([]string{}, fields[:i]...)
			out = append(out, rest...)
			return strings.Join(out, " ")
		}
		return query
	}
	return query
}

// isFormatName reports whether s looks like a ClickHouse output-format name
// (an identifier: letters, digits, underscore).
func isFormatName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// postQuery issues a single HTTP POST with the query as the body (ClickHouse
// HTTP interface) and returns the response body. Client identity/session
// headers (X-ClickHouse-User, X-ClickHouse-Key, Authorization, ...) are
// mirrored 1:1 so authentication passes through transparently; credentials
// are never decoded or logged.
func (e *ClickHouseEvaluator) postQuery(ctx context.Context, uri, query string, headers http.Header) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uri, strings.NewReader(query))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, headers)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

// chEstimateResponse is the FORMAT JSON envelope of EXPLAIN ESTIMATE. Each
// data row carries the estimate for one table (verified against ClickHouse
// 26.8).
type chEstimateResponse struct {
	Data []chEstimateRow `json:"data"`
}

type chEstimateRow struct {
	Database string  `json:"database"`
	Table    string  `json:"table"`
	Parts    float64 `json:"parts"`
	Rows     float64 `json:"rows"`
	Marks    float64 `json:"marks"`
}

// parseExplainEstimate extracts per-table row estimates from an
// EXPLAIN ESTIMATE FORMAT JSON response. Table names are qualified as
// database.table when a database is reported. Rows are summed across tables
// for the query-total estimate.
func parseExplainEstimate(body []byte) ([]TableScan, int64, error) {
	var resp chEstimateResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, 0, err
	}

	var scans []TableScan
	var total int64
	for _, row := range resp.Data {
		name := strings.TrimSpace(row.Table)
		if db := strings.TrimSpace(row.Database); db != "" {
			name = db + "." + name
		}
		if name == "" {
			continue
		}
		rows := int64(row.Rows)
		scans = append(scans, TableScan{Table: name, Rows: rows})
		total += rows
	}
	return scans, total, nil
}
