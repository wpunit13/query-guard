package integration

import (
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
	"query-guard/internal/proxy"
	"query-guard/internal/telemetry"
)

// ──────────────────────────────────────────────────────────────────────────────
// Mock Trino upstream
// ──────────────────────────────────────────────────────────────────────────────

// mockTrino simulates a Trino coordinator. It answers pre-flight EXPLAIN IO
// requests with a configurable plan (delivered via the nextUri polling
// protocol) and records every statement body it receives.
type mockTrino struct {
	server   *httptest.Server
	mu       sync.Mutex
	planJSON string
	received []string
	poll     int
}

func newMockTrino(t *testing.T, planJSON string) *mockTrino {
	m := &mockTrino{planJSON: planJSON}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.received = append(m.received, string(body))
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		// A preflight EXPLAIN POST, or any subsequent GET to a /v1/statement
		// nextUri, belongs to the preflight polling protocol.
		isPreflight := strings.HasPrefix(strings.TrimSpace(string(body)), "EXPLAIN (TYPE IO, FORMAT JSON)") ||
			(r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/statement"))

		if isPreflight {
			// Simulate the Trino V1 protocol: POST → queued, then GETs → running
			// (one carrying the plan cell), then a terminal page.
			m.mu.Lock()
			m.poll++
			step := m.poll
			m.mu.Unlock()
			switch step {
			case 1:
				w.Write([]byte(`{"id":"pre","nextUri":"http://` + r.Host + `/v1/statement/queued/pre/1","stats":{"state":"QUEUED"}}`))
			case 2:
				w.Write([]byte(`{"id":"pre","nextUri":"http://` + r.Host + `/v1/statement/executing/pre/2","stats":{"state":"RUNNING"}}`))
			case 3:
				// Trino delivers the plan as a JSON string inside the data cell.
				cell, _ := json.Marshal(m.planJSON)
				w.Write([]byte(`{"id":"pre","nextUri":"http://` + r.Host + `/v1/statement/executing/pre/3","columns":[{"name":"Query Plan"}],"data":[[` + string(cell) + `]],"stats":{"state":"RUNNING"}}`))
			default:
				w.Write([]byte(`{"id":"pre","stats":{"state":"FINISHED"}}`))
			}
			return
		}

		// Normal query submission response.
		w.Write([]byte(`{"id":"query-1","infoUri":"http://trino/query-1","nextUri":"http://trino/query-1/next"}`))
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockTrino) statements() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.received...)
}

// ──────────────────────────────────────────────────────────────────────────────
// Fixtures
// ──────────────────────────────────────────────────────────────────────────────

const (
	// allowedPlan scans ~1 MiB from hive.default.orders.
	allowedPlan = `{"inputTableColumnInfos":[{"table":{"catalog":"hive","schemaTable":{"schema":"default","table":"orders"}},"estimate":{"outputRowCount":10000.0,"outputSizeInBytes":1048576.0}}]}`
	// blockedPlan scans ~20 MiB, exceeding the 10 MiB per-query limit.
	blockedPlan = `{"inputTableColumnInfos":[{"table":{"catalog":"hive","schemaTable":{"schema":"default","table":"orders"}},"estimate":{"outputRowCount":10000.0,"outputSizeInBytes":20000000.0}}]}`
)

func e2ePolicy(upstreamURL string) *config.Policy {
	return &config.Policy{
		Upstream: config.UpstreamConfig{URL: upstreamURL},
		Preflight: config.PreflightConfig{
			Timeout: 2 * time.Second,
		},
		Rules: config.RulesConfig{
			CostLimits: []config.CostLimit{
				{MaxScanBytesPerQuery: 10_000_000},
			},
		},
		Telemetry: config.TelemetryConfig{
			Enabled: true,
			Path:    "/metrics",
		},
	}
}

// newE2EHandler wires a real TrinoEvaluator + telemetry into a real Handler.
func newE2EHandler(t *testing.T, planJSON string) (*proxy.Handler, *mockTrino, *telemetry.Metrics) {
	mock := newMockTrino(t, planJSON)
	p := e2ePolicy(mock.server.URL)

	cfg := config.NewConfig(p)
	eval := engine.NewTrinoEvaluator(cfg, nil)
	metrics := telemetry.NewMetrics(nil)

	h, err := proxy.NewHandler(cfg, eval, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	h.SetMetrics(metrics)

	return h, mock, metrics
}

func doReq(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	h.ServeHTTP(rec, req)
	return rec
}

// ──────────────────────────────────────────────────────────────────────────────
// End-to-end lifecycle tests
// ──────────────────────────────────────────────────────────────────────────────

func TestE2E_AllowedQueryLifecycle(t *testing.T) {
	h, mock, _ := newE2EHandler(t, allowedPlan)
	rec := doReq(h, http.MethodPost, "/v1/statement", "SELECT * FROM hive.default.orders")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The upstream must have received the original query (not the EXPLAIN).
	// Filter out the empty poll GET bodies that are part of the preflight
	// protocol; only non-empty statement submissions count.
	stmts := mock.statements()
	var real []string
	for _, s := range stmts {
		if strings.TrimSpace(s) != "" {
			real = append(real, s)
		}
	}
	if len(real) != 2 {
		t.Fatalf("upstream received %d statements, want 2 (preflight + real); got %v", len(real), real)
	}
	if !strings.HasPrefix(real[0], "EXPLAIN (TYPE IO, FORMAT JSON)") {
		t.Errorf("first upstream call should be the preflight EXPLAIN, got: %q", real[0])
	}
	if !strings.Contains(real[1], "SELECT * FROM hive.default.orders") {
		t.Errorf("second upstream call should be the real query, got: %q", real[1])
	}
}

func TestE2E_BlockedQueryLifecycle(t *testing.T) {
	h, _, _ := newE2EHandler(t, blockedPlan)

	rec := doReq(h, http.MethodPost, "/v1/statement", "SELECT * FROM hive.default.orders")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "QUERY_GUARD_LIMIT_BREACH") {
		t.Errorf("rejection body missing error_code: %s", rec.Body.String())
	}

	// Metrics must reflect the blocked query and the blocked bytes.
	metricsRec := doReq(h, http.MethodGet, "/metrics", "")
	if metricsRec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", metricsRec.Code)
	}
	body := metricsRec.Body.String()
	if !strings.Contains(body, `queryguard_queries_total{engine="trino",status="blocked"} 1`) {
		t.Errorf("metrics missing blocked counter:\n%s", body)
	}
	if !strings.Contains(body, `queryguard_blocked_bytes_total 2e+07`) {
		t.Errorf("metrics missing blocked bytes (20000000):\n%s", body)
	}
}

func TestE2E_BypassStatement(t *testing.T) {
	h, mock, _ := newE2EHandler(t, allowedPlan)

	rec := doReq(h, http.MethodPost, "/v1/statement", "SHOW TABLES")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Bypass statements should NOT trigger a preflight EXPLAIN.
	if len(mock.statements()) != 1 {
		t.Fatalf("upstream received %d statements, want 1 (no preflight for bypass)", len(mock.statements()))
	}

	metricsRec := doReq(h, http.MethodGet, "/metrics", "")
	if !strings.Contains(metricsRec.Body.String(), `queryguard_queries_total{engine="trino",status="bypassed"} 1`) {
		t.Errorf("metrics missing bypassed counter:\n%s", metricsRec.Body.String())
	}
}

func TestE2E_Healthz(t *testing.T) {
	h, _, _ := newE2EHandler(t, allowedPlan)
	rec := doReq(h, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("healthz body = %q, want it to contain 'ok'", rec.Body.String())
	}
}

func TestE2E_MetricsEndpoint(t *testing.T) {
	h, _, _ := newE2EHandler(t, allowedPlan)

	// Submit a query first so the per-label query counter is initialised and
	// emitted by the Prometheus exposition.
	q := doReq(h, http.MethodPost, "/v1/statement", "SELECT * FROM hive.default.orders")
	if q.Code != http.StatusOK {
		t.Fatalf("setup query status = %d, want 200", q.Code)
	}

	rec := doReq(h, http.MethodGet, "/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", rec.Code)
	}
	for _, want := range []string{
		"queryguard_queries_total",
		"queryguard_blocked_bytes_total",
		"queryguard_preflight_duration_seconds",
		"queryguard_parser_errors_total",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestE2E_PreflightDurationRecorded(t *testing.T) {
	h, _, _ := newE2EHandler(t, allowedPlan)

	doReq(h, http.MethodPost, "/v1/statement", "SELECT * FROM hive.default.orders")

	metricsRec := doReq(h, http.MethodGet, "/metrics", "")
	if !strings.Contains(metricsRec.Body.String(), "queryguard_preflight_duration_seconds_count 1") {
		t.Errorf("preflight histogram count not recorded:\n%s", metricsRec.Body.String())
	}
}
