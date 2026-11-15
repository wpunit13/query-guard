package integration

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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
// End-to-end ClickHouse lifecycle tests
// ──────────────────────────────────────────────────────────────────────────────

// mockClickHouse simulates a ClickHouse HTTP endpoint: EXPLAIN ESTIMATE posts
// get a canned estimate, everything else is a normal query submission.
type mockClickHouse struct {
	server   *httptest.Server
	mu       sync.Mutex
	estimate string
	received []string
}

func newMockClickHouseE2E(t *testing.T, estimate string) *mockClickHouse {
	m := &mockClickHouse{estimate: estimate}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.received = append(m.received, string(body))
		isPreflight := strings.HasPrefix(strings.TrimSpace(string(body)), "EXPLAIN ESTIMATE")
		m.mu.Unlock()

		if isPreflight {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(m.estimate))
			return
		}
		// Normal query submission response (TabSeparated-ish placeholder).
		w.Write([]byte("ok\n"))
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockClickHouse) statements() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.received...)
}

// chE2EPolicy builds a rows-only ClickHouse policy: 5000 rows on default.hits.
func chE2EPolicy(upstreamURL string) *config.Policy {
	return &config.Policy{
		Upstream: config.UpstreamConfig{Engine: "clickhouse", URL: upstreamURL},
		Preflight: config.PreflightConfig{
			Timeout: 2 * time.Second,
		},
		Rules: config.RulesConfig{
			CostLimits: []config.CostLimit{
				{Schema: "default", Table: "hits", MaxRows: 5000},
			},
		},
		Telemetry: config.TelemetryConfig{
			Enabled: true,
			Path:    "/metrics",
		},
	}
}

// newClickHouseE2EHandler wires a real ClickHouseEvaluator (via the engine
// registry) + telemetry into a real Handler.
func newClickHouseE2EHandler(t *testing.T, estimate string) (*proxy.Handler, *mockClickHouse, *telemetry.Metrics) {
	mock := newMockClickHouseE2E(t, estimate)
	p := chE2EPolicy(mock.server.URL)

	cfg := config.NewConfig(p)
	eval, err := engine.New(cfg, p.Upstream.Engine, nil)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	metrics := telemetry.NewMetrics(nil)

	h, err := proxy.NewHandler(cfg, eval, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	h.SetMetrics(metrics)

	return h, mock, metrics
}

// ClickHouse accepts queries on the server root — the handler must route
// POST / through the two-tier pipeline (Engine.StatementPath() == "/").
func TestClickHouseE2E_StatementPathIsRoot(t *testing.T) {
	h, mock, _ := newClickHouseE2EHandler(t, chE2EEstimate(1000))
	rec := doReq(h, http.MethodPost, "/", "SELECT * FROM default.hits")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The pre-flight EXPLAIN ESTIMATE and the original query both reached the
	// upstream; the original query must be forwarded verbatim.
	found := false
	for _, s := range mock.statements() {
		if s == "SELECT * FROM default.hits" {
			found = true
		}
	}
	if !found {
		t.Errorf("original query not forwarded upstream; received: %v", mock.statements())
	}
}

func TestClickHouseE2E_AllowedQuery(t *testing.T) {
	h, _, _ := newClickHouseE2EHandler(t, chE2EEstimate(1000))
	rec := doReq(h, http.MethodPost, "/", "SELECT * FROM default.hits WHERE id = 1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestClickHouseE2E_RowLimitBlocked(t *testing.T) {
	// 20000 estimated rows exceed the 5000-row limit on default.hits.
	h, _, _ := newClickHouseE2EHandler(t, chE2EEstimate(20000))
	rec := doReq(h, http.MethodPost, "/", "SELECT * FROM default.hits")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "QUERY_GUARD_LIMIT_BREACH") {
		t.Errorf("rejection body missing error_code: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rows") {
		t.Errorf("rejection body should mention rows: %s", rec.Body.String())
	}
}

func TestClickHouseE2E_UnparseableEstimateFailsOpen(t *testing.T) {
	// A garbage EXPLAIN ESTIMATE response must forward the query unchanged.
	h, mock, _ := newClickHouseE2EHandler(t, "<<not json>>")
	rec := doReq(h, http.MethodPost, "/", "SELECT * FROM default.hits")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open); body=%s", rec.Code, rec.Body.String())
	}
	found := false
	for _, s := range mock.statements() {
		if s == "SELECT * FROM default.hits" {
			found = true
		}
	}
	if !found {
		t.Errorf("original query not forwarded on fail-open; received: %v", mock.statements())
	}
}

// chE2EEstimate builds an EXPLAIN ESTIMATE FORMAT JSON body for default.hits.
func chE2EEstimate(rows int) string {
	return `{"meta":[{"name":"database","type":"String"},{"name":"table","type":"String"},{"name":"parts","type":"UInt64"},{"name":"rows","type":"UInt64"},{"name":"marks","type":"UInt64"}],"data":[{"database":"default","table":"hits","parts":2,"rows":` +
		strconv.Itoa(rows) + `,"marks":10}],"rows":1,"statistics":{}}`
}
