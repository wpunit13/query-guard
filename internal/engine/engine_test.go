package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"query-guard/internal/config"
	"query-guard/internal/telemetry"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// ──────────────────────────────────────────────────────────────────────────────
// Fixtures
// ──────────────────────────────────────────────────────────────────────────────

// ioPlanReal is a realistic Trino EXPLAIN (TYPE IO, FORMAT JSON) output as
// returned in a data cell: an object with `inputTableColumnInfos`, each entry
// carrying `table.{catalog,schemaTable.schema,table}` and an `estimate` with
// `outputRowCount` / `outputSizeInBytes` (floats).
const ioPlanReal = `{
  "inputTableColumnInfos": [
    {
      "table": {
        "catalog": "hive",
        "schemaTable": {
          "schema": "default",
          "table": "orders"
        }
      },
      "estimate": {
        "outputRowCount": 10000.0,
        "outputSizeInBytes": 1048576.0
      }
    },
    {
      "table": {
        "catalog": "hive",
        "schemaTable": {
          "schema": "default",
          "table": "customers"
        }
      },
      "estimate": {
        "outputRowCount": 5000.0,
        "outputSizeInBytes": 524288.0
      }
    }
  ]
}`

// ioPlanDetails is a legacy/generic operator-tree plan where estimates are
// embedded in each node's `details` string (fallback path).
const ioPlanDetails = `[
  {
    "queryPlan": {
      "root": {
        "name": "Output",
        "identifier": "OutputNode",
        "details": "",
        "children": [
          {
            "name": "TableScan",
            "identifier": "TableScanNode",
            "details": "connectorId=hive, table=hive.default.orders, estimatedDataSizeInBytes=1048576, estimatedRows=10000",
            "children": []
          }
        ]
      }
    }
  }
]`

func testPolicy() *config.Policy {
	return &config.Policy{
		Upstream: config.UpstreamConfig{URL: "http://trino.example:8080"},
		Preflight: config.PreflightConfig{
			Timeout: 2 * time.Second,
		},
		Rules: config.RulesConfig{
			CostLimits: []config.CostLimit{
				{MaxScanBytesPerQuery: 10_000_000},
				{Catalog: "hive", Schema: "default", Table: "orders", MaxScanBytes: 2_000_000, MaxRows: 50_000},
			},
		},
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// parseTrinoIOPlan tests
// ──────────────────────────────────────────────────────────────────────────────

func TestParseTrinoIOPlan_Real(t *testing.T) {
	scanBytes, rows, scans, err := parseTrinoIOPlan([]byte(ioPlanReal))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scanBytes != 1048576+524288 {
		t.Errorf("scanBytes = %d, want %d", scanBytes, 1048576+524288)
	}
	if rows != 10000+5000 {
		t.Errorf("rows = %d, want %d", rows, 15000)
	}
	if len(scans) != 2 {
		t.Fatalf("len(scans) = %d, want 2: %v", len(scans), scans)
	}
}

func TestParseTrinoIOPlan_DetailsFallback(t *testing.T) {
	scanBytes, rows, scans, err := parseTrinoIOPlan([]byte(ioPlanDetails))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scanBytes != 1048576 {
		t.Errorf("scanBytes = %d, want 1048576", scanBytes)
	}
	if rows != 10000 {
		t.Errorf("rows = %d, want 10000", rows)
	}
	if len(scans) != 1 || scans[0].Table != "hive.default.orders" {
		t.Errorf("unexpected scans: %v", scans)
	}
}

func TestParseTrinoIOPlan_InvalidJSON(t *testing.T) {
	if _, _, _, err := parseTrinoIOPlan([]byte("not json")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// evaluateLimits / tableMatches tests
// ──────────────────────────────────────────────────────────────────────────────

// TestEvaluateLimits_NoTableEstimates_RecordsMetric covers B5: when
// table-scoped cost limits are configured but the plan yields no per-table
// estimates, the inert-limit counter must increment (alertable signal).
func TestEvaluateLimits_NoTableEstimates_RecordsMetric(t *testing.T) {
	metrics := telemetry.NewMetrics(nil)
	e := NewTrinoEvaluator(config.NewConfig(testPolicy()), nil)
	e.SetMetrics(metrics)

	allowed, _ := e.evaluateLimits(context.Background(), 1_000_000, 1000, nil)
	if !allowed {
		t.Fatalf("expected fail-open allow, got blocked")
	}
	if got := testutil.ToFloat64(metrics.PreflightNoTableEstimates()); got != 1 {
		t.Errorf("preflight_no_table_estimates_total = %v, want 1", got)
	}

	// With per-table scans present, the counter must NOT move.
	allowed, _ = e.evaluateLimits(context.Background(), 1_000_000, 1000, []TableScan{
		{Table: "hive.default.orders", ScanBytes: 500_000, Rows: 1000},
	})
	if !allowed {
		t.Fatalf("expected allowed, got blocked")
	}
	if got := testutil.ToFloat64(metrics.PreflightNoTableEstimates()); got != 1 {
		t.Errorf("preflight_no_table_estimates_total = %v, want still 1", got)
	}
}

func TestEvaluateLimits_Allowed(t *testing.T) {
	e := NewTrinoEvaluator(config.NewConfig(testPolicy()), nil)
	allowed, reason := e.evaluateLimits(context.Background(), 1_000_000, 1000, []TableScan{
		{Table: "hive.default.orders", ScanBytes: 500_000, Rows: 1000},
	})
	if !allowed {
		t.Fatalf("expected allowed, reason=%q", reason)
	}
}

func TestEvaluateLimits_PerQueryLimitBreach(t *testing.T) {
	e := NewTrinoEvaluator(config.NewConfig(testPolicy()), nil)
	allowed, reason := e.evaluateLimits(context.Background(), 20_000_000, 1000, nil)
	if allowed {
		t.Fatalf("expected blocked, got allowed")
	}
	if !strings.Contains(reason, "per-query limit") {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestEvaluateLimits_TableLimitBreach(t *testing.T) {
	e := NewTrinoEvaluator(config.NewConfig(testPolicy()), nil)
	allowed, reason := e.evaluateLimits(context.Background(), 1_000_000, 1000, []TableScan{
		{Table: "hive.default.orders", ScanBytes: 5_000_000, Rows: 1000},
	})
	if allowed {
		t.Fatalf("expected blocked, got allowed")
	}
	if !strings.Contains(reason, "orders") {
		t.Errorf("unexpected reason: %q", reason)
	}
}

func TestTableMatches(t *testing.T) {
	// Fully-qualified limit: catalog.hive + schema.default + table.orders.
	full := config.CostLimit{Catalog: "hive", Schema: "default", Table: "orders"}
	cases := []struct {
		name  string
		lim   config.CostLimit
		table string
		want  bool
	}{
		{"full match", full, "hive.default.orders", true},
		{"missing catalog", full, "default.orders", false},
		{"bare table", full, "orders", false},
		{"wrong schema", full, "hive.other.orders", false},
		{"wrong table", full, "hive.default.customers", false},
		// Table-only limit matches by the trailing component.
		{"table only", config.CostLimit{Table: "orders"}, "hive.default.orders", true},
		{"table only bare", config.CostLimit{Table: "orders"}, "orders", true},
		{"table only mismatch", config.CostLimit{Table: "orders"}, "hive.default.customers", false},
	}
	for _, tc := range cases {
		if got := tableMatches(tc.table, tc.lim); got != tc.want {
			t.Errorf("tableMatches(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// TrinoEvaluator.Evaluate integration tests (httptest upstream)
// ──────────────────────────────────────────────────────────────────────────────

// trinoMock simulates the Trino V1 statement protocol: a POST returns a
// QUEUED page with a nextUri, then GETs return RUNNING pages and finally a
// page carrying the plan data cell, then a terminal page with null nextUri.
type trinoMock struct {
	planJSON string
	calls    int
}

func (m *trinoMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		switch m.calls {
		case 1: // POST /v1/statement → queued, nextUri
			w.Write([]byte(`{"id":"q1","nextUri":"http://` + r.Host + `/v1/statement/queued/q1/1","stats":{"state":"QUEUED"}}`))
		case 2: // GET → running
			w.Write([]byte(`{"id":"q1","nextUri":"http://` + r.Host + `/v1/statement/executing/q1/2","stats":{"state":"RUNNING"}}`))
		case 3: // GET → running, carries the plan data cell
			// Trino delivers the plan as a JSON string inside the data cell.
			cell, _ := json.Marshal(m.planJSON)
			w.Write([]byte(`{"id":"q1","nextUri":"http://` + r.Host + `/v1/statement/executing/q1/3","columns":[{"name":"Query Plan"}],"data":[[` + string(cell) + `]],"stats":{"state":"RUNNING"}}`))
		case 4: // GET → finished, no nextUri
			w.Write([]byte(`{"id":"q1","stats":{"state":"FINISHED"}}`))
		default:
			w.Write([]byte(`{"id":"q1","stats":{"state":"FINISHED"}}`))
		}
	}
}

func TestEvaluate_Allowed(t *testing.T) {
	mock := &trinoMock{planJSON: ioPlanReal}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	p := testPolicy()
	p.Upstream.URL = srv.URL
	e := NewTrinoEvaluator(config.NewConfig(p), nil)

	hdr := http.Header{}
	hdr.Set("X-Trino-User", "alice")
	hdr.Set("Authorization", "Bearer token")

	res, err := e.Evaluate(context.Background(), "SELECT * FROM orders", hdr)
	if err != nil {
		t.Fatalf("Evaluate() unexpected error: %v", err)
	}
	if !res.Allowed {
		t.Fatalf("expected allowed, reason=%q", res.Reason)
	}
	if res.Engine != "trino" {
		t.Errorf("Engine = %q, want trino", res.Engine)
	}
	if res.EstimatedScanBytes != 1048576+524288 {
		t.Errorf("EstimatedScanBytes = %d, want %d", res.EstimatedScanBytes, 1048576+524288)
	}
	if res.EstimatedRows != 15000 {
		t.Errorf("EstimatedRows = %d, want 15000", res.EstimatedRows)
	}
}

func TestEvaluate_Blocked(t *testing.T) {
	// 2MB scan on hive.default.orders exceeds the 2MB table limit.
	big := `{"inputTableColumnInfos":[{"table":{"catalog":"hive","schemaTable":{"schema":"default","table":"orders"}},"estimate":{"outputRowCount":20000.0,"outputSizeInBytes":2097152.0}}]}`
	mock := &trinoMock{planJSON: big}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	p := testPolicy()
	p.Upstream.URL = srv.URL
	e := NewTrinoEvaluator(config.NewConfig(p), nil)

	res, err := e.Evaluate(context.Background(), "SELECT * FROM orders", http.Header{})
	if err != nil {
		t.Fatalf("Evaluate() unexpected error: %v", err)
	}
	if res.Allowed {
		t.Fatalf("expected blocked, got allowed")
	}
}

func TestEvaluate_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := testPolicy()
	p.Upstream.URL = srv.URL
	e := NewTrinoEvaluator(config.NewConfig(p), nil)

	if _, err := e.Evaluate(context.Background(), "SELECT 1", http.Header{}); err == nil {
		t.Fatal("expected error for non-200 status")
	}
}

func TestEvaluate_TimeoutFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep well beyond the evaluator timeout.
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := testPolicy()
	p.Upstream.URL = srv.URL
	p.Preflight.Timeout = 50 * time.Millisecond
	e := NewTrinoEvaluator(config.NewConfig(p), nil)

	start := time.Now()
	_, err := e.Evaluate(context.Background(), "SELECT 1", http.Header{})
	if err == nil {
		t.Fatal("expected timeout error (fail-open signal)")
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Errorf("Evaluate() took too long: %v", time.Since(start))
	}
}

func TestEvaluate_ConcurrencyGate(t *testing.T) {
	// The first call blocks on this channel, holding the single slot.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"stats":{"state":"FINISHED"}}`))
	}))
	defer srv.Close()

	p := testPolicy()
	p.Upstream.URL = srv.URL
	p.Preflight.MaxConcurrent = 1
	p.Preflight.Timeout = 500 * time.Millisecond
	e := NewTrinoEvaluator(config.NewConfig(p), nil)

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
