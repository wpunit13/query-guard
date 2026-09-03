package config

import (
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
}

// RequiredFilter enforces that a specific column appears in the WHERE clause.
type RequiredFilter struct {
	Catalog string `yaml:"catalog"`
	Schema  string `yaml:"schema"`
	Table   string `yaml:"table"`
	Column  string `yaml:"column"`
}

// CostLimit defines maximum scan bytes and/or rows per table scope.
type CostLimit struct {
	Catalog              string `yaml:"catalog"`
	Schema               string `yaml:"schema"`
	Table                string `yaml:"table"`
	MaxScanBytes         int64  `yaml:"max_scan_bytes"`
	MaxRows              int64  `yaml:"max_rows"`
	MaxScanBytesPerQuery int64  `yaml:"max_scan_bytes_per_query"`
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
	if err := yaml.Unmarshal(data, &p); err != nil {
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
