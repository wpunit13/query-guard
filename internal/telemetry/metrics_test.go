package telemetry

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRecordQuery_InitializesCounter(t *testing.T) {
	m := NewMetrics(nil)
	m.RecordQuery(StatusAllowed, "trino")
	m.RecordQuery(StatusBlocked, "trino")

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`queryguard_queries_total{engine="trino",status="allowed"} 1`,
		`queryguard_queries_total{engine="trino",status="blocked"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestRecordBlockedBytes(t *testing.T) {
	m := NewMetrics(nil)
	m.RecordBlockedBytes(20000000)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "queryguard_blocked_bytes_total 2e+07") {
		t.Errorf("blocked bytes not recorded:\n%s", rec.Body.String())
	}
}

func TestObservePreflightAndParserErrors(t *testing.T) {
	m := NewMetrics(nil)
	m.ObservePreflight(150 * time.Millisecond)
	m.RecordParserError()
	m.RecordParserError()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "queryguard_preflight_duration_seconds_count 1") {
		t.Errorf("preflight histogram not recorded:\n%s", body)
	}
	if !strings.Contains(body, "queryguard_parser_errors_total 2") {
		t.Errorf("parser error counter not recorded:\n%s", body)
	}
}

func TestNilMetricsSafe(t *testing.T) {
	var m *Metrics
	// These must not panic / must be no-ops.
	m.RecordQuery(StatusAllowed, "trino")
	m.RecordBlockedBytes(5)
	m.ObservePreflight(time.Second)
	m.RecordParserError()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 404 {
		t.Errorf("nil metrics handler code = %d, want 404", rec.Code)
	}
}
