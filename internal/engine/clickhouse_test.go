package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"query-guard/internal/config"
)

// ──────────────────────────────────────────────────────────────────────────────
// ClickHouse evaluator tests
// ──────────────────────────────────────────────────────────────────────────────

// mockClickHouse simulates a ClickHouse HTTP endpoint. It answers EXPLAIN
// ESTIMATE posts with a canned response and records requests.
type mockClickHouse struct {
	server   *httptest.Server
	mu       sync.Mutex
	response string
	status   int
	received []string
	headers  []http.Header
}

func newMockClickHouse(t *testing.T, response string, status int) *mockClickHouse {
	m := &mockClickHouse{response: response, status: status}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.received = append(m.received, string(body))
		m.headers = append(m.headers, r.Header.Clone())
		m.mu.Unlock()

		if status != http.StatusOK {
			w.WriteHeader(status)
			w.Write([]byte("error"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(m.response))
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockClickHouse) lastRequest() (string, http.Header) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.received) == 0 {
		return "", nil
	}
	return m.received[len(m.received)-1], m.headers[len(m.headers)-1]
}

// ──────────────────────────────────────────────────────────────────────────────
// Shadow FORMAT-clause collision
// ──────────────────────────────────────────────────────────────────────────────

func TestStripTrailingFormatClause(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"SELECT * FROM hits FORMAT JSON", "SELECT * FROM hits"},
		{"SELECT * FROM hits FORMAT CSVWithNames", "SELECT * FROM hits"},
		{"select * from hits format json", "select * from hits"},
		{"SELECT * FROM hits FORMAT JSON SETTINGS max_threads = 1", "SELECT * FROM hits SETTINGS max_threads = 1"},
		// No trailing FORMAT: unchanged.
		{"SELECT * FROM hits", "SELECT * FROM hits"},
		{"SELECT * FROM hits SETTINGS max_threads = 1", "SELECT * FROM hits SETTINGS max_threads = 1"},
		// FORMAT inside a string literal is not a clause.
		{"SELECT 'FORMAT JSON' FROM hits", "SELECT 'FORMAT JSON' FROM hits"},
		{"SELECT x FROM hits WHERE y = 'FORMAT JSON'", "SELECT x FROM hits WHERE y = 'FORMAT JSON'"},
		// Dangling FORMAT (invalid query): unchanged.
		{"SELECT * FROM hits FORMAT", "SELECT * FROM hits FORMAT"},
	}
	for _, tc := range cases {
		if got := stripTrailingFormatClause(tc.in); got != tc.want {
			t.Errorf("stripTrailingFormatClause(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClickHouseShadowStripsQueryFormat(t *testing.T) {
	m := newMockClickHouse(t, `{"data":[{"database":"default","table":"hits","parts":1,"rows":10,"marks":1}]}`, http.StatusOK)
	cfg := config.NewConfig(&config.Policy{
		Upstream:  config.UpstreamConfig{Engine: "clickhouse", URL: m.server.URL},
		Preflight: config.PreflightConfig{Timeout: 2 * time.Second, MaxConcurrent: 2},
		Rules:     config.RulesConfig{CostLimits: []config.CostLimit{{Schema: "default", Table: "hits", MaxRows: 100}}},
	})
	e := NewClickHouseEvaluator(cfg, nil)

	// The client query carries its own FORMAT clause; the shadow must not
	// double it (which would be a syntax error and fail open).
	_, err := e.Evaluate(context.Background(), "SELECT * FROM default.hits FORMAT JSON", nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	shadow, _ := m.lastRequest()
	want := "EXPLAIN ESTIMATE SELECT * FROM default.hits FORMAT JSON"
	if shadow != want {
		t.Errorf("shadow = %q, want %q", shadow, want)
	}
}

// chEstimateJSON builds an EXPLAIN ESTIMATE FORMAT JSON body (26.8 shape:
// database, table, parts, rows, marks per data row).
func chEstimateJSON(rows ...string) string {
	return `{"meta":[{"name":"database","type":"String"},{"name":"table","type":"String"},{"name":"parts","type":"UInt64"},{"name":"rows","type":"UInt64"},{"name":"marks","type":"UInt64"}],"data":[` +
		strings.Join(rows, ",") + `],"rows":` + strconv.Itoa(len(rows)) + `,"statistics":{}}`
}

func newClickHouseEval(t *testing.T, p *config.Policy) *ClickHouseEvaluator {
	t.Helper()
	return NewClickHouseEvaluator(config.NewConfig(p), nil)
}

func TestClickHouse_StatementPath(t *testing.T) {
	e := newClickHouseEval(t, testPolicy())
	if got := e.StatementPath(); got != "/" {
		t.Errorf("StatementPath() = %q, want /", got)
	}
	if got := e.Engine(); got != "clickhouse" {
		t.Errorf("Engine() = %q, want clickhouse", got)
	}
}

func TestNew_ClickHouseRegistered(t *testing.T) {
	e, err := New(config.NewConfig(testPolicy()), "clickhouse", nil)
	if err != nil {
		t.Fatalf("New(clickhouse) unexpected error: %v", err)
	}
	if got := e.StatementPath(); got != "/" {
		t.Errorf("StatementPath() = %q, want /", got)
	}
}

func TestClickHouse_Evaluate_Allowed(t *testing.T) {
	// 1000 rows on default.hits, limit 5000 → allowed.
	mock := newMockClickHouse(t, chEstimateJSON(`{"database":"default","table":"hits","parts":2,"rows":1000,"marks":10}`), http.StatusOK)
	p := testPolicy()
	p.Upstream.URL = mock.server.URL
	p.Rules.CostLimits = []config.CostLimit{{Schema: "default", Table: "hits", MaxRows: 5000}}
	e := newClickHouseEval(t, p)

	res, err := e.Evaluate(context.Background(), "SELECT * FROM hits", http.Header{})
	if err != nil {
		t.Fatalf("Evaluate unexpected error: %v", err)
	}
	if !res.Allowed {
		t.Fatalf("expected allowed, reason=%q", res.Reason)
	}
	if res.EstimatedRows != 1000 {
		t.Errorf("EstimatedRows = %d, want 1000", res.EstimatedRows)
	}
	// ClickHouse has no byte or CPU estimate: both must stay 0 so the shared
	// limits skip them (rows-only enforcement).
	if res.EstimatedScanBytes != 0 || res.EstimatedCPUCost != 0 {
		t.Errorf("bytes/cpu = %d/%g, want 0/0", res.EstimatedScanBytes, res.EstimatedCPUCost)
	}
	if len(res.TableScans) != 1 || res.TableScans[0].Table != "default.hits" || res.TableScans[0].Rows != 1000 {
		t.Errorf("TableScans = %+v, want [default.hits 1000 rows]", res.TableScans)
	}
}

func TestClickHouse_Evaluate_RowLimitBlocked(t *testing.T) {
	// 20000 rows on default.hits, limit 5000 → blocked with the table named.
	mock := newMockClickHouse(t, chEstimateJSON(`{"database":"default","table":"hits","parts":2,"rows":20000,"marks":10}`), http.StatusOK)
	p := testPolicy()
	p.Upstream.URL = mock.server.URL
	p.Rules.CostLimits = []config.CostLimit{{Schema: "default", Table: "hits", MaxRows: 5000}}
	e := newClickHouseEval(t, p)

	res, err := e.Evaluate(context.Background(), "SELECT * FROM hits", http.Header{})
	if err != nil {
		t.Fatalf("Evaluate unexpected error: %v", err)
	}
	if res.Allowed {
		t.Fatal("expected blocked, got allowed")
	}
	if !strings.Contains(res.Reason, "hits") || !strings.Contains(res.Reason, "rows") {
		t.Errorf("reason = %q, want it to name the table and rows", res.Reason)
	}
}

func TestClickHouse_Evaluate_HeadersMirrored(t *testing.T) {
	mock := newMockClickHouse(t, chEstimateJSON(), http.StatusOK)
	p := testPolicy()
	p.Upstream.URL = mock.server.URL
	e := newClickHouseEval(t, p)

	hdr := http.Header{}
	hdr.Set("X-ClickHouse-User", "alice")
	hdr.Set("X-ClickHouse-Key", "secret")
	hdr.Set("Authorization", "Bearer token")
	if _, err := e.Evaluate(context.Background(), "SELECT 1", hdr); err != nil {
		t.Fatalf("Evaluate unexpected error: %v", err)
	}

	body, got := mock.lastRequest()
	if !strings.HasPrefix(body, "EXPLAIN ESTIMATE SELECT 1 FORMAT JSON") {
		t.Errorf("shadow query = %q, want EXPLAIN ESTIMATE prefix + FORMAT JSON suffix", body)
	}
	for k, want := range map[string]string{
		"X-ClickHouse-User": "alice",
		"X-ClickHouse-Key":  "secret",
		"Authorization":     "Bearer token",
	} {
		if got.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, got.Get(k), want)
		}
	}
}

func TestClickHouse_Evaluate_NonOKStatusFailsOpen(t *testing.T) {
	mock := newMockClickHouse(t, "error", http.StatusInternalServerError)
	p := testPolicy()
	p.Upstream.URL = mock.server.URL
	e := newClickHouseEval(t, p)

	if _, err := e.Evaluate(context.Background(), "SELECT 1", http.Header{}); err == nil {
		t.Fatal("expected error for non-200 status (fail-open signal)")
	}
}

func TestClickHouse_Evaluate_UnparseableResponseFailsOpen(t *testing.T) {
	mock := newMockClickHouse(t, "not json at all", http.StatusOK)
	p := testPolicy()
	p.Upstream.URL = mock.server.URL
	e := newClickHouseEval(t, p)

	if _, err := e.Evaluate(context.Background(), "SELECT 1", http.Header{}); err == nil {
		t.Fatal("expected error for unparseable response (fail-open signal)")
	}
}

func TestClickHouse_Evaluate_TimeoutFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := testPolicy()
	p.Upstream.URL = srv.URL
	p.Preflight.Timeout = 50 * time.Millisecond
	e := newClickHouseEval(t, p)

	if _, err := e.Evaluate(context.Background(), "SELECT 1", http.Header{}); err == nil {
		t.Fatal("expected timeout error (fail-open signal)")
	}
}

func TestClickHouse_Evaluate_ConcurrencyGate(t *testing.T) {
	// The first call blocks on this channel, holding the single slot.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.Write([]byte(chEstimateJSON()))
	}))
	defer srv.Close()

	p := testPolicy()
	p.Upstream.URL = srv.URL
	p.Preflight.MaxConcurrent = 1
	p.Preflight.Timeout = 500 * time.Millisecond
	e := newClickHouseEval(t, p)

	// First call acquires the single slot and blocks.
	done := make(chan struct{})
	go func() {
		_, _ = e.Evaluate(context.Background(), "SELECT 1", http.Header{})
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // ensure the first call acquired the slot

	// Second call gets a short deadline, so it times out waiting for a slot
	// and fails open with ErrPreflightConcurrency.
	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := e.Evaluate(shortCtx, "SELECT 2", http.Header{})
	if !errors.Is(err, ErrPreflightConcurrency) {
		t.Fatalf("expected ErrPreflightConcurrency, got %v", err)
	}

	close(block) // release the first call
	<-done
}

func TestParseExplainEstimate_MultiTableAndEmpty(t *testing.T) {
	scans, total, err := parseExplainEstimate([]byte(chEstimateJSON(
		`{"database":"default","table":"hits","parts":1,"rows":100,"marks":1}`,
		`{"database":"db","table":"users","parts":1,"rows":23,"marks":1}`,
	)))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if total != 123 {
		t.Errorf("total rows = %d, want 123", total)
	}
	if len(scans) != 2 || scans[0].Table != "default.hits" || scans[1].Table != "db.users" {
		t.Errorf("scans = %+v, want default.hits + db.users", scans)
	}

	// No tables (e.g. SELECT 1): empty scans, zero rows, no error.
	scans, total, err = parseExplainEstimate([]byte(`{"data":[],"rows":0}`))
	if err != nil || len(scans) != 0 || total != 0 {
		t.Errorf("empty estimate: scans=%v total=%d err=%v", scans, total, err)
	}
}
