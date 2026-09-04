package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// Load / Validation Tests
// ──────────────────────────────────────────────────────────────────────────────

func TestLoad_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, validPolicyYAML())

	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if p.Server.Port != 8080 {
		t.Errorf("expected port 8080, got %d", p.Server.Port)
	}
	if p.Upstream.URL != "http://trino-coordinator:8080" {
		t.Errorf("expected upstream URL, got %q", p.Upstream.URL)
	}
	if p.Preflight.Timeout != 2*time.Second {
		t.Errorf("expected preflight timeout 2s, got %v", p.Preflight.Timeout)
	}
	if p.Preflight.MaxConcurrent != 5 {
		t.Errorf("expected max_concurrent 5, got %d", p.Preflight.MaxConcurrent)
	}
	if len(p.Rules.CostLimits) != 1 {
		t.Errorf("expected 1 cost_limit, got %d", len(p.Rules.CostLimits))
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/policy.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, "::: not yaml {{")

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestLoad_InvalidPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, invalidPortYAML())

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid port, got nil")
	}
}

func TestLoad_EmptyURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, emptyURLYAML())

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty upstream url, got nil")
	}
}

func TestLoad_NoRules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, noRulesYAML())

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for policy with no rules, got nil")
	}
}

func TestDefaults_Applied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, minimalValidYAML())

	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if p.Server.ReadTimeout != 30*time.Second {
		t.Errorf("expected default read_timeout 30s, got %v", p.Server.ReadTimeout)
	}
	if p.Server.WriteTimeout != 5*time.Minute {
		t.Errorf("expected default write_timeout 5m, got %v", p.Server.WriteTimeout)
	}
	if p.Server.ShutdownGrace != 10*time.Second {
		t.Errorf("expected default shutdown_grace 10s, got %v", p.Server.ShutdownGrace)
	}
	if p.Upstream.Timeout != 30*time.Second {
		t.Errorf("expected default upstream timeout 30s, got %v", p.Upstream.Timeout)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// v1 Guard-Enhancement Config Tests (audit, function/star blocklists, CPU
// cost cap, required-filter schema)
// ──────────────────────────────────────────────────────────────────────────────

func TestV1Config_NewFieldsParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, `
server:
  port: 8080

upstream:
  url: "http://trino:8080"

audit:
  log_rejections: true
  snapshot_plan: true

rules:
  function_blocklist: [regexp_count, regexp_extract]
  select_star_blocked_tables: [wide_table, events]
  required_filters:
    - catalog: finance
      schema: reporting
      table: daily_orders
      mode: any-of
      columns: [ds, month, year]
  cost_limits:
    - max_cpu_cost_per_query: 500.0
`)

	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if !p.Audit.LogRejections || !p.Audit.SnapshotPlan {
		t.Errorf("audit config not parsed: %+v", p.Audit)
	}
	if len(p.Rules.FunctionBlocklist) != 2 || len(p.Rules.SelectStarBlockedTables) != 2 {
		t.Errorf("blocklists not parsed: %+v", p.Rules)
	}
	rf := p.Rules.RequiredFilters[0]
	if rf.Mode != "any-of" || len(rf.Columns) != 3 {
		t.Errorf("required filter not parsed: %+v", rf)
	}
	if p.Rules.CostLimits[0].MaxCPUCostPerQuery != 500.0 {
		t.Errorf("cpu cost limit not parsed: %+v", p.Rules.CostLimits[0])
	}
}

func TestV1Config_RequiredFilterValidation(t *testing.T) {
	cases := map[string]string{
		"missing schema": `
rules:
  required_filters:
    - table: daily_orders
      columns: [ds]
`,
		"missing table": `
rules:
  required_filters:
    - schema: reporting
      columns: [ds]
`,
		"empty columns": `
rules:
  required_filters:
    - schema: reporting
      table: daily_orders
      columns: []
`,
		"invalid mode": `
rules:
  required_filters:
    - schema: reporting
      table: daily_orders
      mode: some-of
      columns: [ds]
`,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "policy.yaml")
			writeFile(t, path, "upstream:\n  url: \"http://trino:8080\"\n"+yaml)

			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected validation error for %s, got nil", name)
			}
		})
	}
}

func TestV1Config_NegativeCPUCostRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, `
upstream:
  url: "http://trino:8080"

rules:
  cost_limits:
    - max_cpu_cost_per_query: -1.0
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for negative max_cpu_cost_per_query, got nil")
	}
}

func TestV1Config_UnknownFieldRejected(t *testing.T) {
	// Strict decode: the removed singular `column` field of required_filters
	// must fail loudly rather than silently no-op (breaking schema change).
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, `
upstream:
  url: "http://trino:8080"

rules:
  required_filters:
    - schema: reporting
      table: daily_orders
      column: ds
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for removed `column` field, got nil")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// TLS Config Tests
// ──────────────────────────────────────────────────────────────────────────────

func TestTLS_PartialConfigRejected(t *testing.T) {
	cases := map[string]string{
		"cert without key": `
server:
  port: 8080
  tls:
    cert_file: /etc/query-guard/tls/tls.crt

upstream:
  url: "http://trino:8080"

rules:
  table_blocklist: [t]
`,
		"key without cert": `
server:
  port: 8080
  tls:
    key_file: /etc/query-guard/tls/tls.key

upstream:
  url: "http://trino:8080"

rules:
  table_blocklist: [t]
`,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "policy.yaml")
			writeFile(t, path, yaml)

			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error for partial TLS config, got nil")
			}
			if !strings.Contains(err.Error(), "cert_file and key_file") {
				t.Errorf("error %q does not mention cert_file/key_file pairing", err)
			}
		})
	}
}

func TestTLS_FullConfigAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, `
server:
  port: 8080
  tls:
    cert_file: /etc/query-guard/tls/tls.crt
    key_file: /etc/query-guard/tls/tls.key

upstream:
  url: "https://trino:8443"

rules:
  table_blocklist: [t]
`)

	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if !p.Server.TLS.Enabled() {
		t.Error("expected TLS.Enabled() == true with cert and key configured")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Config Wrapper Tests
// ──────────────────────────────────────────────────────────────────────────────

func TestConfig_GetAndSet(t *testing.T) {
	p1 := &Policy{Server: ServerConfig{Port: 8080}}
	cfg := NewConfig(p1)

	got1 := cfg.Get()
	if got1.Server.Port != 8080 {
		t.Errorf("expected port 8080, got %d", got1.Server.Port)
	}

	p2 := &Policy{Server: ServerConfig{Port: 9090}}
	cfg.Set(p2)

	got2 := cfg.Get()
	if got2.Server.Port != 9090 {
		t.Errorf("expected port 9090 after Set, got %d", got2.Server.Port)
	}

	// Original pointer must not be mutated
	if p1.Server.Port != 8080 {
		t.Errorf("original policy was mutated; port changed to %d", p1.Server.Port)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// YAML Helpers
// ──────────────────────────────────────────────────────────────────────────────

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %q: %v", path, err)
	}
}

func validPolicyYAML() string {
	return `
server:
  port: 8080

upstream:
  url: "http://trino-coordinator:8080"

preflight:
  timeout: 2s
  max_concurrent: 5

rules:
  cost_limits:
    - catalog: "hive"
      schema: "default"
      table: "orders"
      max_scan_bytes: 1073741824
      max_rows: 10000000

telemetry:
  enabled: true
  path: "/metrics"
`
}

func invalidPortYAML() string {
	return `
server:
  port: 99999

upstream:
  url: "http://trino:8080"

rules:
  cost_limits:
    - catalog: "hive"
      schema: "default"
      table: "orders"
      max_scan_bytes: 1073741824
`
}

func emptyURLYAML() string {
	return `
server:
  port: 8080

upstream:
  url: ""

rules:
  cost_limits:
    - catalog: "hive"
      schema: "default"
      table: "orders"
      max_scan_bytes: 1073741824
`
}

func noRulesYAML() string {
	return `
server:
  port: 8080

upstream:
  url: "http://trino:8080"

rules:
  cost_limits: []
`
}

func minimalValidYAML() string {
	return `
server:
  port: 8080

upstream:
  url: "http://trino:8080"

rules:
  statement_blocklist:
    - "ALTER"
    - "DROP"
`
}
