package config

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ──────────────────────────────────────────────────────────────────────────────
// Policy — top-level YAML model
// ──────────────────────────────────────────────────────────────────────────────

// Policy is the complete configuration model for query-guard.
type Policy struct {
	Server    ServerConfig    `yaml:"server"`
	Upstream  UpstreamConfig  `yaml:"upstream"`
	Preflight PreflightConfig `yaml:"preflight"`
	Rules     RulesConfig     `yaml:"rules"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
	Audit     AuditConfig     `yaml:"audit"`
}

// AuditConfig controls opt-in audit logging of rejected queries.
type AuditConfig struct {
	// LogRejections enables one structured audit log line per rejected
	// query (rejections only; allowed/bypassed queries are never audited).
	LogRejections bool `yaml:"log_rejections"`
	// SnapshotPlan includes the Tier-2 cost snapshot (estimated bytes,
	// rows, CPU cost, per-table scans) in the audit line for Tier-2
	// rejections. Has no effect unless LogRejections is true.
	SnapshotPlan bool `yaml:"snapshot_plan"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Sub-structs
// ──────────────────────────────────────────────────────────────────────────────

// ServerConfig holds the HTTP listener settings.
type ServerConfig struct {
	Port          int           `yaml:"port"`
	ReadTimeout   time.Duration `yaml:"read_timeout"`
	WriteTimeout  time.Duration `yaml:"write_timeout"`
	ShutdownGrace time.Duration `yaml:"shutdown_grace_period"`
	TLS           TLSConfig     `yaml:"tls"`
}

// TLSConfig configures native HTTPS on the proxy listener. When CertFile and
// KeyFile are both set the server serves HTTPS only (no plaintext listener),
// so Authorization / X-Trino-* headers are never transmitted in the clear.
// The certificate material itself lives in a mounted K8s Secret; only the
// file paths belong in policy.yaml.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Enabled reports whether native TLS is configured.
func (t TLSConfig) Enabled() bool { return t.CertFile != "" && t.KeyFile != "" }

// UpstreamConfig defines the target database cluster to proxy to.
type UpstreamConfig struct {
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"timeout"`
}

// PreflightConfig controls Tier 2 behaviour (shadow cost evaluation).
type PreflightConfig struct {
	Timeout       time.Duration `yaml:"timeout"`
	MaxConcurrent int           `yaml:"max_concurrent"`
}

// RulesConfig contains all static and cost-based enforcement rules.
type RulesConfig struct {
	TableBlocklist     []string         `yaml:"table_blocklist"`
	RequiredFilters    []RequiredFilter `yaml:"required_filters"`
	CostLimits         []CostLimit      `yaml:"cost_limits"`
	StatementBlocklist []string         `yaml:"statement_blocklist"`
	// FunctionBlocklist lists function/UDF names (case-insensitive) that may
	// not appear anywhere in a statement. Empty = disabled.
	FunctionBlocklist []string `yaml:"function_blocklist"`
	// SelectStarBlockedTables lists tables on which `SELECT *` is rejected
	// (case-insensitive; explicit column lists are fine). Empty = disabled.
	SelectStarBlockedTables []string `yaml:"select_star_blocked_tables"`
}

// RequiredFilter enforces that specific columns appear in the WHERE clause
// of queries against a specific schema.table.
type RequiredFilter struct {
	// Catalog is optional; empty matches any catalog.
	Catalog string `yaml:"catalog"`
	// Schema and Table are required: the entry applies only to that exact
	// schema.table (no suffix/bare-name matching).
	Schema string `yaml:"schema"`
	Table  string `yaml:"table"`
	// Mode is "any-of" (at least one column filtered) or "all-of" (all
	// columns filtered). Empty defaults to all-of.
	Mode string `yaml:"mode"`
	// Columns lists the column names that must be filtered on, attributed
	// to this specific table.
	Columns []string `yaml:"columns"`
}

// CostLimit defines maximum scan bytes, rows, and CPU cost per scope.
type CostLimit struct {
	Catalog              string `yaml:"catalog"`
	Schema               string `yaml:"schema"`
	Table                string `yaml:"table"`
	MaxScanBytes         int64  `yaml:"max_scan_bytes"`
	MaxRows              int64  `yaml:"max_rows"`
	MaxScanBytesPerQuery int64  `yaml:"max_scan_bytes_per_query"`
	// MaxCPUCostPerQuery is a global per-query cap on the estimated CPU cost
	// from the EXPLAIN plan's root estimate. Global-only: CPU is not
	// attributable per-table across joins. 0 = disabled.
	MaxCPUCostPerQuery float64 `yaml:"max_cpu_cost_per_query"`
}

// TelemetryConfig controls observability endpoints.
type TelemetryConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Thread-Safe Config Wrapper (hot-reload target)
// ──────────────────────────────────────────────────────────────────────────────

// Config wraps a Policy pointer with a RWMutex so it can be atomically swapped
// by the file watcher without blocking concurrent readers.
type Config struct {
	mu     sync.RWMutex
	policy *Policy
}

// NewConfig creates a Config from an already-loaded Policy.
func NewConfig(p *Policy) *Config {
	return &Config{policy: p}
}

// Get returns a read-locked pointer to the current Policy.
func (c *Config) Get() *Policy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.policy
}

// Set atomically replaces the inner Policy (called by Watcher on hot-reload).
func (c *Config) Set(p *Policy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policy = p
}

// ──────────────────────────────────────────────────────────────────────────────
// Load / Validate / Defaults
// ──────────────────────────────────────────────────────────────────────────────

// Load reads, parses, applies defaults, and validates a policy YAML file.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: cannot read %q: %w", path, err)
	}

	var p Policy
	// Strict decode: unknown fields (including the removed singular `column`
	// of required_filters) fail loudly instead of silently degrading —
	// important because the required_filters schema change is breaking.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("config: YAML parse error in %q: %w", path, err)
	}

	setDefaults(&p)

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("config: validation failed in %q: %w", path, err)
	}

	return &p, nil
}

// setDefaults fills zero-valued fields with sensible production defaults.
func setDefaults(p *Policy) {
	if p.Server.Port == 0 {
		p.Server.Port = 8080
	}
	if p.Server.ReadTimeout == 0 {
		p.Server.ReadTimeout = 30 * time.Second
	}
	if p.Server.WriteTimeout == 0 {
		p.Server.WriteTimeout = 5 * time.Minute
	}
	if p.Server.ShutdownGrace == 0 {
		p.Server.ShutdownGrace = 10 * time.Second
	}
	if p.Upstream.Timeout == 0 {
		p.Upstream.Timeout = 30 * time.Second
	}
	if p.Preflight.Timeout == 0 {
		p.Preflight.Timeout = 2 * time.Second
	}
	if p.Preflight.MaxConcurrent == 0 {
		p.Preflight.MaxConcurrent = 5
	}
	if p.Telemetry.Path == "" {
		p.Telemetry.Path = "/metrics"
	}
}

// Validate checks that required fields are present and rules are coherent.
func (p *Policy) Validate() error {
	if p.Server.Port <= 0 || p.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", p.Server.Port)
	}
	if p.Upstream.URL == "" {
		return fmt.Errorf("upstream.url must not be empty")
	}
	if len(p.Rules.CostLimits) == 0 &&
		len(p.Rules.TableBlocklist) == 0 &&
		len(p.Rules.RequiredFilters) == 0 &&
		len(p.Rules.StatementBlocklist) == 0 {
		return fmt.Errorf("at least one rule category (cost_limits, table_blocklist, required_filters, or statement_blocklist) must be populated")
	}

	// Cost-limit values must be non-negative. A value of 0 disables that
	// specific limit (documented semantics), so only negatives are invalid.
	for _, lim := range p.Rules.CostLimits {
		if lim.MaxScanBytes < 0 || lim.MaxRows < 0 || lim.MaxScanBytesPerQuery < 0 {
			return fmt.Errorf("cost limit values must be >= 0 (0 disables the limit), got %+v", lim)
		}
		if lim.MaxCPUCostPerQuery < 0 {
			return fmt.Errorf("max_cpu_cost_per_query must be >= 0 (0 disables the limit), got %v", lim.MaxCPUCostPerQuery)
		}
	}

	// Required filters: schema+table scoping is mandatory (breaking change
	// from the old suffix-matched singular-column form), mode is restricted,
	// and at least one column must be listed.
	for i, rf := range p.Rules.RequiredFilters {
		if rf.Schema == "" || rf.Table == "" {
			return fmt.Errorf("required_filters[%d]: schema and table are required (entry matches exactly one schema.table)", i)
		}
		switch rf.Mode {
		case "", "any-of", "all-of":
			// valid; empty mode defaults to all-of at enforcement time
		default:
			return fmt.Errorf("required_filters[%d]: mode must be \"any-of\" or \"all-of\" (or empty for all-of), got %q", i, rf.Mode)
		}
		if len(rf.Columns) == 0 {
			return fmt.Errorf("required_filters[%d]: at least one entry in columns is required", i)
		}
		for _, c := range rf.Columns {
			if c == "" {
				return fmt.Errorf("required_filters[%d]: columns must not contain empty names", i)
			}
		}
	}

	// TLS must be either fully configured or fully absent: a cert without a
	// key (or vice versa) would otherwise fail only at listen time, after the
	// process has started.
	tlsSet := 0
	if p.Server.TLS.CertFile != "" {
		tlsSet++
	}
	if p.Server.TLS.KeyFile != "" {
		tlsSet++
	}
	if tlsSet == 1 {
		return fmt.Errorf("server.tls: both cert_file and key_file must be set to enable TLS (got only one)")
	}
	return nil
}
