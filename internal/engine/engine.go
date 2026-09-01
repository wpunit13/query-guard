package engine

import (
	"context"
	"net/http"
)

// ──────────────────────────────────────────────────────────────────────────────
// CostEvaluator — pluggable pre-flight cost evaluation interface
// ──────────────────────────────────────────────────────────────────────────────

// CostEvaluator is the generic interface for Tier 2 pre-flight cost checks.
// Implementations issue a shadow cost-evaluation query (e.g. Trino's
// `EXPLAIN (TYPE IO, FORMAT JSON)`) against the upstream engine and decide
// whether the incoming query is safe to run.
//
// The proxy handler depends only on this interface, so a new engine (e.g.
// Spark SQL) can be added by writing a new struct that satisfies it without
// touching the handler.
//
// Implementations MUST be fail-open: when an evaluation cannot be completed
// (timeout, upstream error, unparseable plan) they return a non-nil error so
// the caller can forward the original query rather than cause downtime.
type CostEvaluator interface {
	// Evaluate runs a pre-flight cost check for the given SQL query, mirroring
	// the client headers 1:1 onto the shadow request. It returns a CostResult
	// describing whether the query is allowed and the estimated resource usage.
	Evaluate(ctx context.Context, query string, headers http.Header) (CostResult, error)

	// Engine returns the engine name (e.g. "trino") used for telemetry labels.
	Engine() string
}

// ──────────────────────────────────────────────────────────────────────────────
// Result types
// ──────────────────────────────────────────────────────────────────────────────

// TableScan is a per-table estimate extracted from a cost-evaluation plan.
type TableScan struct {
	// Table is the fully-qualified table name (catalog.schema.table) as
	// reported by the engine, used for table-scoped limit matching.
	Table string
	// ScanBytes is the estimated number of bytes scanned from this table.
	ScanBytes int64
	// Rows is the estimated number of rows scanned from this table.
	Rows int64
}

// CostResult is the outcome of a pre-flight cost evaluation.
type CostResult struct {
	// Allowed is true when the query is within configured limits.
	Allowed bool

	// EstimatedScanBytes is the total estimated bytes scanned across all
	// tables referenced by the query.
	EstimatedScanBytes int64

	// EstimatedRows is the total estimated rows scanned across all tables.
	EstimatedRows int64

	// TableScans holds per-table estimates for table-scoped limit matching.
	TableScans []TableScan

	// Reason is a human-readable explanation of a rejection (empty when
	// Allowed is true).
	Reason string

	// Engine identifies the CostEvaluator implementation that produced the
	// result (e.g. "trino"). Used for telemetry labels.
	Engine string
}
