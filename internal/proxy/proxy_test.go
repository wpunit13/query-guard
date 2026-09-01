package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"query-guard/internal/config"
	"query-guard/internal/engine"
)

// ──────────────────────────────────────────────────────────────────────────────
// Test doubles
// ──────────────────────────────────────────────────────────────────────────────

// fakeEvaluator is a configurable engine.CostEvaluator for tests.
type fakeEvaluator struct {
	result      engine.CostResult
	err         error
	mu          sync.Mutex
	called      int
	lastQuery   string
	lastHeaders http.Header
}

func (f *fakeEvaluator) Evaluate(_ context.Context, query string, headers http.Header) (engine.CostResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	f.lastQuery = query
	f.lastHeaders = headers.Clone()
	if f.err != nil {
		return engine.CostResult{Engine: "fake"}, f.err
	}
	return f.result, nil
}

func (f *fakeEvaluator) Engine() string {
	return "fake"
}

func (f *fakeEvaluator) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.called
}

// upstreamRecorder is an httptest server that records the last request it
// received and responds with a fixed status.
type upstreamRecorder struct {
	server *httptest.Server
	mu     sync.Mutex
	status int
	body   string
	path   string
	method string
	auth   string
}

func newUpstreamRecorder(t *testing.T, status int) *upstreamRecorder {
	u := &upstreamRecorder{status: status}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.body = string(b)
		u.path = r.URL.Path
		u.method = r.Method
		u.auth = r.Header.Get("Authorization")
		u.mu.Unlock()
		w.WriteHeader(u.status)
		w.Write([]byte("ok"))
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *upstreamRecorder) snapshot() (method, path, body, auth string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.method, u.path, u.body, u.auth
}

// newTestHandler wires a Handler against a recording upstream.
func newTestHandler(t *testing.T, policy *config.Policy, eval engine.CostEvaluator) (*Handler, *upstreamRecorder) {
	u := newUpstreamRecorder(t, http.StatusOK)
	policy.Upstream.URL = u.server.URL

	h, err := NewHandler(config.NewConfig(policy), eval, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, u
}

func testPolicy() *config.Policy {
	return &config.Policy{
		Upstream: config.UpstreamConfig{URL: "http://unused:8080"},
		Preflight: config.PreflightConfig{
			Timeout: 2 * time.Second,
		},
		Rules: config.RulesConfig{
			CostLimits: []config.CostLimit{
				{MaxScanBytesPerQuery: 10_000_000, Table: "orders", MaxRows: 100_000},
			},
		},
	}
}

func doRequest(h *Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// ──────────────────────────────────────────────────────────────────────────────
// Tests
// ──────────────────────────────────────────────────────────────────────────────

func TestBypassEndpointProxied(t *testing.T) {
	h, u := newTestHandler(t, testPolicy(), nil)
	rec := doRequest(h, http.MethodGet, "/v1/info", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (bypass endpoint should be proxied)", rec.Code)
	}
	method, path, _, _ := u.snapshot()
	if method != http.MethodGet || path != "/v1/info" {
		t.Errorf("upstream saw %s %s, want GET /v1/info", method, path)
	}
}

func TestUpdateUpstream_HotReload(t *testing.T) {
	// Two recording upstreams; the handler starts on the first.
	u1 := newUpstreamRecorder(t, http.StatusOK)
	u2 := newUpstreamRecorder(t, http.StatusOK)
	p := testPolicy()
	p.Upstream.URL = u1.server.URL

	h, err := NewHandler(config.NewConfig(p), nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	// Initial request goes to u1.
	doRequest(h, http.MethodGet, "/v1/info", "", nil)
	if _, path, _, _ := u1.snapshot(); path != "/v1/info" {
		t.Errorf("initial request did not reach u1, got path %q", path)
	}

	// Hot-swap the upstream to u2 and confirm subsequent requests go there.
	if err := h.UpdateUpstream(u2.server.URL); err != nil {
		t.Fatalf("UpdateUpstream: %v", err)
	}
	doRequest(h, http.MethodGet, "/v1/info", "", nil)
	if _, path, _, _ := u2.snapshot(); path != "/v1/info" {
		t.Errorf("request after UpdateUpstream did not reach u2, got path %q", path)
	}
}

func TestBypassStatementForwarded(t *testing.T) {
	h, u := newTestHandler(t, testPolicy(), &fakeEvaluator{})
	rec := doRequest(h, http.MethodPost, statementPath, "SHOW TABLES", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (bypass statement should forward)", rec.Code)
	}
	_, _, body, _ := u.snapshot()
	if !strings.Contains(body, "SHOW TABLES") {
		t.Errorf("upstream body = %q, want it to contain the query", body)
	}
}

func TestStatementAllowedAndHeaderPassthrough(t *testing.T) {
	eval := &fakeEvaluator{result: engine.CostResult{Allowed: true, Engine: "fake", EstimatedScanBytes: 100}}
	h, u := newTestHandler(t, testPolicy(), eval)

	rec := doRequest(h, http.MethodPost, statementPath, "SELECT * FROM orders", map[string]string{
		"Authorization": "Bearer abc123",
		"X-Trino-User":  "alice",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if eval.calls() != 1 {
		t.Fatalf("evaluator called %d times, want 1", eval.calls())
	}
	if eval.lastHeaders.Get("Authorization") != "Bearer abc123" {
		t.Errorf("evaluator did not receive Authorization header: %v", eval.lastHeaders)
	}
	_, _, body, auth := u.snapshot()
	if !strings.Contains(body, "SELECT * FROM orders") {
		t.Errorf("upstream did not receive original query body: %q", body)
	}
	if auth != "Bearer abc123" {
		t.Errorf("upstream auth header = %q, want passthrough", auth)
	}
}

func TestStatementBlockedByTableBlocklist(t *testing.T) {
	p := testPolicy()
	p.Rules.TableBlocklist = []string{"blocked_tbl"}
	h, u := newTestHandler(t, p, nil)

	rec := doRequest(h, http.MethodPost, statementPath, "SELECT * FROM blocked_tbl", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertRejection(t, rec, ReasonTableBlocklist)
	_, _, _, _ = u.snapshot()
}

func TestStatementBlockedMissingRequiredFilter(t *testing.T) {
	p := testPolicy()
	p.Rules.RequiredFilters = []config.RequiredFilter{
		{Catalog: "hive", Schema: "analytics", Table: "orders", Column: "partition_dt"},
	}
	h, u := newTestHandler(t, p, nil)

	// Missing required filter → reject.
	rec := doRequest(h, http.MethodPost, statementPath, "SELECT * FROM hive.analytics.orders", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing filter: status = %d, want 400", rec.Code)
	}
	assertRejection(t, rec, ReasonRequiredFilter)

	// Filter present → allowed.
	rec2 := doRequest(h, http.MethodPost, statementPath, "SELECT * FROM hive.analytics.orders WHERE partition_dt = '2024-01-01'", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("with filter: status = %d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
	_, _, _, _ = u.snapshot()
}

func TestRequiredFilter_NotSatisfiedByOtherTable(t *testing.T) {
	p := testPolicy()
	p.Rules.RequiredFilters = []config.RequiredFilter{
		{Catalog: "hive", Schema: "analytics", Table: "orders", Column: "orderdate"},
	}
	h, u := newTestHandler(t, p, nil)

	// orderdate appears only qualified with the *other* table (t) in the join,
	// so it must NOT satisfy orders' required filter.
	rec := doRequest(h, http.MethodPost, statementPath,
		"SELECT * FROM hive.analytics.orders o JOIN hive.analytics.customers t ON t.orderdate = o.orderkey", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (other-table filter must not satisfy required filter); body=%s", rec.Code, rec.Body.String())
	}
	assertRejection(t, rec, ReasonRequiredFilter)

	// A filter qualified with the guarded table's alias satisfies it.
	rec2 := doRequest(h, http.MethodPost, statementPath,
		"SELECT * FROM hive.analytics.orders o JOIN hive.analytics.customers t ON t.id = o.orderkey WHERE o.orderdate = DATE '2024-01-01'", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("qualified filter: status = %d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
	_, _, _, _ = u.snapshot()
}

func TestTableBlocklisted_CaseInsensitive(t *testing.T) {
	p := testPolicy()
	p.Rules.TableBlocklist = []string{"LINEITEM"}
	h, u := newTestHandler(t, p, nil)

	rec := doRequest(h, http.MethodPost, statementPath, "SELECT * FROM tpch.tiny.lineitem", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (case-insensitive blocklist match); body=%s", rec.Code, rec.Body.String())
	}
	assertRejection(t, rec, ReasonTableBlocklist)
	_, _, _, _ = u.snapshot()
}

func TestOversizedBody_FailsOpen(t *testing.T) {
	h, u := newTestHandler(t, testPolicy(), nil)

	big := strings.Repeat("a", maxStatementBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, statementPath, strings.NewReader(big))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Oversized bodies are forwarded un-truncated (fail-open), not rejected.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open forward); body=%s", rec.Code, rec.Body.String())
	}
	_, _, body, _ := u.snapshot()
	if len(body) != len(big) {
		t.Errorf("upstream received %d bytes, want %d (must not truncate)", len(body), len(big))
	}
}

func TestReadyz_UpstreamDown(t *testing.T) {
	p := testPolicy()
	p.Upstream.URL = "http://127.0.0.1:1" // nothing listening
	h, err := NewHandler(config.NewConfig(p), nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	rec := doRequest(h, http.MethodGet, "/readyz", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503 when upstream is down", rec.Code)
	}
}

func TestStatementRejectedByStatementBlocklist(t *testing.T) {
	p := testPolicy()
	p.Rules.StatementBlocklist = []string{"CREATE"}
	h, u := newTestHandler(t, p, nil)

	rec := doRequest(h, http.MethodPost, statementPath, "CREATE TABLE t (id INT)", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertRejection(t, rec, ReasonStatementBlocklist)
	_, _, _, _ = u.snapshot()
}

func TestPreflightLimitBreachRejects(t *testing.T) {
	eval := &fakeEvaluator{result: engine.CostResult{Allowed: false, Engine: "fake", Reason: "estimated scan bytes 20M exceeds limit 10M"}}
	h, u := newTestHandler(t, testPolicy(), eval)

	rec := doRequest(h, http.MethodPost, statementPath, "SELECT * FROM orders", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertRejection(t, rec, ReasonCostLimitBreach)
	_, _, _, _ = u.snapshot()
}

func TestPreflightFailOpenOnError(t *testing.T) {
	eval := &fakeEvaluator{err: context.DeadlineExceeded}
	h, u := newTestHandler(t, testPolicy(), eval)

	rec := doRequest(h, http.MethodPost, statementPath, "SELECT * FROM orders", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open on evaluator error)", rec.Code)
	}
	_, _, body, _ := u.snapshot()
	if !strings.Contains(body, "SELECT * FROM orders") {
		t.Errorf("upstream body = %q, want original query forwarded", body)
	}
}

func TestPreflightNotRunForMutatingStatement(t *testing.T) {
	eval := &fakeEvaluator{}
	h, u := newTestHandler(t, testPolicy(), eval)

	rec := doRequest(h, http.MethodPost, statementPath, "INSERT INTO orders (id) VALUES (1)", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if eval.calls() != 0 {
		t.Errorf("evaluator.called = %d, want 0 (mutating statements skip preflight)", eval.calls())
	}
	_, _, _, _ = u.snapshot()
}

// ──────────────────────────────────────────────────────────────────────────────
// Response helpers
// ──────────────────────────────────────────────────────────────────────────────

func assertRejection(t *testing.T, rec *httptest.ResponseRecorder, wantReason RejectionReason) {
	t.Helper()
	var resp rejectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("rejection body is not valid JSON: %v; body=%s", err, rec.Body.String())
	}
	if resp.ErrorCode != errorCodeLimitBreach {
		t.Errorf("error_code = %q, want %q", resp.ErrorCode, errorCodeLimitBreach)
	}
	if resp.Reason != wantReason {
		t.Errorf("reason = %q, want %q (body=%s)", resp.Reason, wantReason, rec.Body.String())
	}
}
