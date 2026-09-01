package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"query-guard/internal/config"
)

// ──────────────────────────────────────────────────────────────────────────────
// TrinoEvaluator — first CostEvaluator implementation
// ──────────────────────────────────────────────────────────────────────────────

const (
	trinoEngineName = "trino"
	// explainPrefix wraps the incoming query in a shadow I/O cost-evaluation.
	explainPrefix = "EXPLAIN (TYPE IO, FORMAT JSON) "
	// statementPath is the Trino coordinator endpoint that accepts queries.
	statementPath = "/v1/statement"
)

// ErrPreflightConcurrency is returned when the pre-flight concurrency gate is
// saturated and the request times out waiting for a slot. The caller treats it
// as a fail-open signal (forward the query) and may surface a saturation metric.
var ErrPreflightConcurrency = errors.New("engine/trino: preflight concurrency limit reached")

// TrinoEvaluator implements CostEvaluator for Trino clusters. It issues a
// shadow `EXPLAIN (TYPE IO, FORMAT JSON) <query>` request to the upstream
// coordinator (mirroring all client headers 1:1) and parses the returned plan
// to estimate scan bytes and rows.
type TrinoEvaluator struct {
	cfg         *config.Config
	upstreamURL string
	client      *http.Client
	timeout     time.Duration
	sem         chan struct{}
	logger      *log.Logger
}

// NewTrinoEvaluator builds a TrinoEvaluator from the shared, hot-reloadable
// config. A nil client yields a default client. The upstream URL and timeout
// are read from cfg on every Evaluate so policy hot-reloads take effect
// immediately. The pre-flight concurrency gate is sized from
// preflight.max_concurrent.
func NewTrinoEvaluator(cfg *config.Config, client *http.Client) *TrinoEvaluator {
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

	return &TrinoEvaluator{
		cfg:         cfg,
		upstreamURL: strings.TrimRight(p.Upstream.URL, "/"),
		client:      client,
		timeout:     timeout,
		sem:         make(chan struct{}, maxConcurrent),
		logger:      log.New(os.Stderr, "[engine/trino] ", log.LstdFlags),
	}
}

// Engine returns the engine name for telemetry labels.
func (e *TrinoEvaluator) Engine() string {
	return trinoEngineName
}

// currentUpstream returns the up-to-date upstream base URL from the live
// config, so an upstream change is picked up without a restart.
func (e *TrinoEvaluator) currentUpstream() string {
	if e.cfg == nil {
		return e.upstreamURL
	}
	return strings.TrimRight(e.cfg.Get().Upstream.URL, "/")
}

// Evaluate issues a shadow EXPLAIN (TYPE IO, FORMAT JSON) request and evaluates
// the estimated resource usage against configured limits.
//
// Fail-open: any error (timeout, upstream failure, unparseable plan) is
// returned so the caller can forward the original query.
func (e *TrinoEvaluator) Evaluate(ctx context.Context, query string, headers http.Header) (CostResult, error) {
	result := CostResult{Engine: trinoEngineName}

	// Read the upstream and timeout from the live config so hot-reloads apply.
	upstream := e.currentUpstream()
	timeout := e.timeout
	if e.cfg != nil {
		if t := e.cfg.Get().Preflight.Timeout; t > 0 {
			timeout = t
		}
	}

	// Enforce a strict timeout on the shadow request. On expiry the context
	// error is returned so the caller can fail open.
	evalCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Acquire the pre-flight concurrency gate. If the gate is saturated and we
	// cannot get a slot before the timeout, fail open (return an error) rather
	// than queueing unboundedly and amplifying load on the coordinator.
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-evalCtx.Done():
		return result, fmt.Errorf("%w: %v", ErrPreflightConcurrency, evalCtx.Err())
	}

	shadow := explainPrefix + query

	// Trino's protocol returns the query output only after polling the
	// nextUri(s) returned by the initial POST. Collect the plan cells.
	cells, err := e.runStatement(evalCtx, upstream+statementPath, http.MethodPost, shadow, headers)
	if err != nil {
		return result, fmt.Errorf("engine/trino: preflight statement: %w", err)
	}

	scanBytes, rows, scans, err := parsePlanCells(cells)
	if err != nil {
		return result, fmt.Errorf("engine/trino: parse plan: %w", err)
	}

	result.EstimatedScanBytes = scanBytes
	result.EstimatedRows = rows
	result.TableScans = scans

	result.Allowed, result.Reason = e.evaluateLimits(scanBytes, rows, scans)
	return result, nil
}

// trinoResponse is a single response page from the Trino V1 statement protocol.
// NextURI uses a pointer so we can distinguish an explicit null (final page)
// from a missing field.
type trinoResponse struct {
	ID      string          `json:"id"`
	InfoURI string          `json:"infoUri"`
	NextURI *string         `json:"nextUri"`
	Data    [][]interface{} `json:"data"`
	Stats   *trinoStats     `json:"stats"`
	Error   json.RawMessage `json:"error"`
}

type trinoStats struct {
	State string `json:"state"`
}

// runStatement submits a Trino statement (e.g. an EXPLAIN) and polls the
// returned nextUri chain until the query finishes, collecting the data cells
// from each page. Client identity/session headers (X-Trino-User, X-Trino-Catalog,
// Authorization, etc.) are mirrored onto every request so authentication passes
// through transparently.
func (e *TrinoEvaluator) runStatement(ctx context.Context, startURI, method, body string, headers http.Header) ([]string, error) {
	uri := startURI
	curMethod := method
	curBody := body
	var cells []string

	for {
		var reqBody io.Reader
		if curMethod == http.MethodPost {
			reqBody = strings.NewReader(curBody)
		}

		req, err := http.NewRequestWithContext(ctx, curMethod, uri, reqBody)
		if err != nil {
			return nil, err
		}
		if curMethod == http.MethodPost {
			req.Header.Set("Content-Type", "text/plain")
		}
		copyHeaders(req.Header, headers)

		resp, err := e.client.Do(req)
		if err != nil {
			return nil, err
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			// Trino may respond with an HTML error page (e.g. 406 for
			// X-Forwarded-For) or a JSON error body; include a bounded slice.
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}

		var tr trinoResponse
		if err := json.Unmarshal(respBody, &tr); err != nil {
			return nil, fmt.Errorf("decode trino page: %w", err)
		}

		for _, row := range tr.Data {
			if len(row) > 0 {
				if s, ok := row[0].(string); ok {
					cells = append(cells, s)
				}
			}
		}

		if len(tr.Error) > 0 && string(tr.Error) != "null" {
			return nil, fmt.Errorf("trino query error: %s", string(tr.Error))
		}

		if tr.Stats != nil {
			switch tr.Stats.State {
			case "FAILED", "CANCELED", "UNKNOWN":
				return nil, fmt.Errorf("trino query state %s", tr.Stats.State)
			}
		}

		// Trino marks a page FINISHED while still pointing at a final nextUri
		// that carries the last data page, so the terminal condition is a null
		// nextUri, not the FINISHED state.
		if tr.NextURI == nil {
			return cells, nil
		}

		uri = *tr.NextURI
		curMethod = http.MethodGet
		curBody = ""
	}
}

// evaluateLimits checks estimated usage against the configured cost limits.
// It returns (allowed, reason). A non-empty reason explains a rejection.
func (e *TrinoEvaluator) evaluateLimits(totalScanBytes, totalRows int64, scans []TableScan) (bool, string) {
	p := e.cfg.Get()

	// Detect table-scoped limits that cannot be enforced because the plan
	// produced no per-table estimates (aggregate-only plan or fallback path).
	hasTableLimits := false
	for _, lim := range p.Rules.CostLimits {
		if lim.Catalog != "" || lim.Schema != "" || lim.Table != "" {
			hasTableLimits = true
			break
		}
	}
	if hasTableLimits && len(scans) == 0 {
		e.logger.Printf("warning: table-scoped cost limits configured but no per-table scan estimates were produced; table limits are not enforced")
	}

	for _, lim := range p.Rules.CostLimits {
		// Query-level global limit.
		if lim.MaxScanBytesPerQuery > 0 && totalScanBytes > lim.MaxScanBytesPerQuery {
			return false, fmt.Sprintf("estimated scan bytes %d exceeds per-query limit %d",
				totalScanBytes, lim.MaxScanBytesPerQuery)
		}

		// Table-scoped limits require a matching scanned table.
		if lim.Catalog != "" || lim.Schema != "" || lim.Table != "" {
			for _, s := range scans {
				if !tableMatches(s.Table, lim) {
					continue
				}
				if lim.MaxScanBytes > 0 && s.ScanBytes > lim.MaxScanBytes {
					return false, fmt.Sprintf("table %s estimated scan bytes %d exceeds limit %d",
						s.Table, s.ScanBytes, lim.MaxScanBytes)
				}
				if lim.MaxRows > 0 && s.Rows > lim.MaxRows {
					return false, fmt.Sprintf("table %s estimated rows %d exceeds limit %d",
						s.Table, s.Rows, lim.MaxRows)
				}
			}
		}
	}

	return true, ""
}

// tableMatches reports whether a fully-qualified scanned table name matches a
// table-scoped CostLimit. Matching is suffix-based on the rightmost components
// so catalog.schema.table, schema.table, and bare table names all work.
func tableMatches(scanTable string, lim config.CostLimit) bool {
	parts := strings.Split(scanTable, ".")

	if lim.Table != "" && parts[len(parts)-1] != lim.Table {
		return false
	}
	if lim.Schema != "" {
		if len(parts) < 2 || parts[len(parts)-2] != lim.Schema {
			return false
		}
	}
	if lim.Catalog != "" {
		if len(parts) < 3 || parts[len(parts)-3] != lim.Catalog {
			return false
		}
	}
	return true
}

// copyHeaders mirrors client identity/session headers onto the pre-flight
// request to preserve transparent auth passthrough (Authorization, X-Trino-*,
// etc). It deliberately skips transport-level headers that Go's HTTP client
// manages itself (Content-Length, Host, Content-Type) and hop-by-hop headers
// (Connection, Transfer-Encoding, ...), which must not be duplicated or
// mismatched on a freshly-built request.
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isManagedHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// isManagedHeader reports whether a header is owned by the HTTP transport or
// is a hop-by-hop header that must not be copied onto the pre-flight request.
func isManagedHeader(key string) bool {
	switch strings.ToLower(key) {
	case "host", "content-length", "content-type", "transfer-encoding",
		"connection", "keep-alive", "proxy-connection", "te", "trailer",
		"upgrade", "proxy-authenticate", "proxy-authorization",
		"accept-encoding", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto":
		return true
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────────
// Trino I/O plan parsing
// ──────────────────────────────────────────────────────────────────────────────

var (
	reScanBytes = regexp.MustCompile(`estimatedDataSizeInBytes=(\d+)`)
	reRows      = regexp.MustCompile(`estimatedRows=(\d+)`)
	reTable     = regexp.MustCompile(`table=([^,\s]+)`)
)

// parseTrinoIOPlan extracts estimates from a plan body. Kept for tests that
// model a plan returned directly (no Trino protocol envelope); production code
// uses parsePlanCells on the data cells collected from the nextUri chain.
func parseTrinoIOPlan(body []byte) (scanBytes, rows int64, scans []TableScan, err error) {
	return parsePlanCells([]string{string(body)})
}

// parsePlanCells aggregates estimates across one or more plan JSON documents
// (each is a data cell harvested from Trino's EXPLAIN IO output).
func parsePlanCells(cells []string) (scanBytes, rows int64, scans []TableScan, err error) {
	agg := &planAggregate{scans: make(map[string]*TableScan)}
	for _, c := range cells {
		if err := parsePlanDoc([]byte(c), agg); err != nil {
			return 0, 0, nil, err
		}
	}

	// Flatten per-table scans into a deterministic slice.
	for _, s := range agg.scans {
		scans = append(scans, *s)
	}

	return agg.scanBytes, agg.rows, scans, nil
}

// parsePlanDoc walks a single plan document accumulating estimates into agg.
// It understands two shapes:
//  1. Trino's modern I/O plan: an object with `inputTableColumnInfos`, each
//     carrying `table.{catalog,schemaTable.schema,table}` and an `estimate`
//     with `outputRowCount` / `outputSizeInBytes` (floats).
//  2. A generic operator tree (JSON array of trees or a `queryPlan` root),
//     walked recursively as a fallback.
func parsePlanDoc(body []byte, agg *planAggregate) error {
	var io trinoIOPlan
	if err := json.Unmarshal(body, &io); err == nil && len(io.InputTableColumnInfos) > 0 {
		for _, info := range io.InputTableColumnInfos {
			name := qualifiedTable(info.Table.Catalog, info.Table.SchemaTable.Schema, info.Table.SchemaTable.Table)
			// Trino reports these as floats; truncate to int64. Acceptable for
			// realistic sizes, but note float64 loses precision beyond ~2^53.
			sb := int64(info.Estimate.OutputSizeInBytes)
			rows := int64(info.Estimate.OutputRowCount)

			agg.scanBytes += sb
			agg.rows += rows

			ts := agg.scans[name]
			if ts == nil {
				ts = &TableScan{Table: name}
				agg.scans[name] = ts
			}
			ts.ScanBytes += sb
			ts.Rows += rows
		}
		return nil
	}

	// Fallback: generic operator-tree walk (details-string / numeric-key forms).
	var plans []json.RawMessage
	if err := json.Unmarshal(body, &plans); err != nil {
		// Tolerate a single top-level object not wrapped in an array.
		var single json.RawMessage
		if err2 := json.Unmarshal(body, &single); err2 != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		plans = []json.RawMessage{single}
	}

	for _, p := range plans {
		var plan struct {
			QueryPlan json.RawMessage `json:"queryPlan"`
		}
		if err := json.Unmarshal(p, &plan); err != nil {
			walkPlanNode(p, agg)
			continue
		}
		walkPlanNode(plan.QueryPlan, agg)
	}
	return nil
}

// trinoIOPlan models Trino's EXPLAIN (TYPE IO, FORMAT JSON) output.
type trinoIOPlan struct {
	InputTableColumnInfos []trinoTableColumnInfo `json:"inputTableColumnInfos"`
}

type trinoTableColumnInfo struct {
	Table struct {
		Catalog     string `json:"catalog"`
		SchemaTable struct {
			Schema string `json:"schema"`
			Table  string `json:"table"`
		} `json:"schemaTable"`
	} `json:"table"`
	Estimate trinoEstimate `json:"estimate"`
}

type trinoEstimate struct {
	OutputRowCount    float64 `json:"outputRowCount"`
	OutputSizeInBytes float64 `json:"outputSizeInBytes"`
}

// qualifiedTable joins a catalog/schema/table into a dotted name, omitting
// empty leading components.
func qualifiedTable(catalog, schema, table string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{catalog, schema, table} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ".")
}

// planAggregate accumulates totals and per-table estimates during the walk.
type planAggregate struct {
	scanBytes int64
	rows      int64
	scans     map[string]*TableScan
}

// walkPlanNode recursively walks a plan node (object or array) accumulating
// estimates. It recognizes both numeric estimate keys and `details` strings.
func walkPlanNode(raw json.RawMessage, agg *planAggregate) {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return
	}
	walkValue(v, agg)
}

func walkValue(v interface{}, agg *planAggregate) {
	switch val := v.(type) {
	case map[string]interface{}:
		var nodeScanBytes, nodeRows int64

		// Estimates may appear as numeric JSON keys directly on the node.
		if sb, ok := numericKey(val, "estimatedDataSizeInBytes"); ok {
			nodeScanBytes += sb
		}
		if r, ok := numericKey(val, "estimatedRows"); ok {
			nodeRows += r
		}

		nodeName := ""
		if n, ok := val["name"].(string); ok {
			nodeName = n
		}

		// Estimates may also be embedded in the node's details string.
		if d, ok := val["details"].(string); ok {
			sb, r := parseDetailsEstimates(d)
			nodeScanBytes += sb
			nodeRows += r
		}

		agg.scanBytes += nodeScanBytes
		agg.rows += nodeRows

		// Record a per-table scan for TableScan nodes.
		if nodeName == "TableScan" {
			if d, ok := val["details"].(string); ok {
				if table := extractTable(d); table != "" {
					ts := agg.scans[table]
					if ts == nil {
						ts = &TableScan{Table: table}
						agg.scans[table] = ts
					}
					ts.ScanBytes += nodeScanBytes
					ts.Rows += nodeRows
				}
			}
		}

		for _, child := range val {
			walkValue(child, agg)
		}

	case []interface{}:
		for _, item := range val {
			walkValue(item, agg)
		}
	}
}

// parseDetailsEstimates extracts scan-bytes and rows estimates from a plan
// node's details string (the format used by Trino I/O plans).
func parseDetailsEstimates(details string) (scanBytes, rows int64) {
	if m := reScanBytes.FindStringSubmatch(details); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			scanBytes += n
		}
	}
	if m := reRows.FindStringSubmatch(details); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			rows += n
		}
	}
	return scanBytes, rows
}

// extractTable pulls the qualified table name out of a TableScan node's
// details string.
func extractTable(details string) string {
	if m := reTable.FindStringSubmatch(details); m != nil {
		return m[1]
	}
	return ""
}

// numericKey reads a numeric value (float64 or json.Number) from a JSON object.
func numericKey(m map[string]interface{}, key string) (int64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	}
	return 0, false
}
