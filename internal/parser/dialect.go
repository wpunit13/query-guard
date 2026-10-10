package parser

import "sync"

// ──────────────────────────────────────────────────────────────────────────────
// Dialect — Tier 1 seam (per-engine SQL analysis)
// ──────────────────────────────────────────────────────────────────────────────

// Dialect extracts guard-relevant metadata from a SQL statement written in one
// engine's SQL variant. The proxy handler depends only on this interface,
// never on a concrete dialect; the dialect is resolved from the engine name
// via For.
//
// Implementations MUST be fail-open: when a statement cannot be fully parsed
// they return the partial AnalysisResult (e.g. raw table extraction) rather
// than an error that would block the query — a safety tool must never cause
// platform downtime.
type Dialect interface {
	// Name returns the engine name the dialect serves (e.g. "trino").
	Name() string

	// Analyze parses sql and returns the guard-relevant metadata (tables,
	// WHERE columns, functions, projection stars, statement class).
	Analyze(sql string) (*AnalysisResult, error)
}

// Dialects register themselves from their own file via init(), so adding an
// engine never requires editing this file. Registering a nil dialect, an
// empty name, or the same name twice is a programming error and panics at
// startup.
var (
	dialectMu      sync.RWMutex
	dialectsByName = map[string]Dialect{}
)

// Register adds a dialect under the given engine name.
func Register(name string, d Dialect) {
	dialectMu.Lock()
	defer dialectMu.Unlock()

	if name == "" {
		panic("parser: Register called with empty name")
	}
	if d == nil {
		panic("parser: Register called with nil dialect for " + name)
	}
	if _, dup := dialectsByName[name]; dup {
		panic("parser: Register called twice for " + name)
	}
	dialectsByName[name] = d
}

// For resolves the dialect for an engine name (upstream.engine). Unknown
// names fall back to the trino dialect (fail-open: an engine whose dialect is
// not linked into the binary still gets Tier 1 coverage rather than none).
// The returned Dialect is never nil.
func For(engine string) Dialect {
	dialectMu.RLock()
	defer dialectMu.RUnlock()

	if d, ok := dialectsByName[engine]; ok {
		return d
	}
	if d, ok := dialectsByName[trinoDialectName]; ok {
		return d
	}
	// Unreachable in practice: trino registers via init in this package, but
	// return a value directly so For can never yield a nil Dialect.
	return trinoDialect{}
}
