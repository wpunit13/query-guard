package engine

import (
	"fmt"
	"net/http"
	"sync"

	"query-guard/internal/config"
	"query-guard/internal/telemetry"
)

// ──────────────────────────────────────────────────────────────────────────────
// Engine registry — New(cfg, name, client) factory
// ──────────────────────────────────────────────────────────────────────────────

// Constructor builds the Engine implementation for one engine name. It is
// registered from the engine's own file via init(), so adding an engine never
// requires editing the factory.
type Constructor func(cfg *config.Config, client *http.Client) (Engine, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Constructor{}
)

// Register adds an engine constructor under the given name (the
// upstream.engine value, e.g. "trino"). Call it from the engine's own file:
//
//	func init() {
//	    engine.Register("clickhouse", func(cfg *config.Config, client *http.Client) (engine.Engine, error) {
//	        return NewClickHouseEvaluator(cfg, client), nil
//	    })
//	}
//
// Registering a nil constructor, an empty name, or the same name twice is a
// programming error and panics at startup.
func Register(name string, ctor Constructor) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if name == "" {
		panic("engine: Register called with empty name")
	}
	if ctor == nil {
		panic("engine: Register called with nil constructor for " + name)
	}
	if _, dup := registry[name]; dup {
		panic("engine: Register called twice for " + name)
	}
	registry[name] = ctor
}

// New builds the Engine selected by name (upstream.engine). The name is
// validated against the registry, not the config enum, so an engine that is
// configured but never linked into the binary fails loudly here instead of
// silently degrading.
func New(cfg *config.Config, name string, client *http.Client) (Engine, error) {
	registryMu.RLock()
	ctor, ok := registry[name]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("engine: unknown engine %q (registered: build the engine into the binary and register it)", name)
	}
	eng, err := ctor(cfg, client)
	if err != nil {
		return nil, fmt.Errorf("engine: constructing %q: %w", name, err)
	}
	return eng, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Optional capabilities
// ──────────────────────────────────────────────────────────────────────────────

// MetricsSetter is implemented by engines that support telemetry. It is kept
// out of the Engine interface so engines without metrics stay compliant.
type MetricsSetter interface {
	SetMetrics(m *telemetry.Metrics)
}

// SetMetrics attaches a telemetry sink to an engine that supports one.
// Engines without telemetry ignore the call, so callers (main.go) stay
// engine-agnostic.
func SetMetrics(eng Engine, m *telemetry.Metrics) {
	if s, ok := eng.(MetricsSetter); ok {
		s.SetMetrics(m)
	}
}
