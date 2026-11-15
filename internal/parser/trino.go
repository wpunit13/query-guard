package parser

// ──────────────────────────────────────────────────────────────────────────────
// Trino dialect — Tier 1 adapter over the shared token extractor
// ──────────────────────────────────────────────────────────────────────────────

// trinoDialectName is the engine name served by trinoDialect.
const trinoDialectName = "trino"

// trinoKeywords lists the Trino clause keywords beyond the shared ANSI list
// that the extractor must never mistake for tables, aliases, columns, or
// function names — e.g. the `FETCH FIRST n ROWS ONLY` result suffix and
// `NULLS FIRST/LAST` ordering modifiers.
var trinoKeywords = &DialectKeywords{
	ExtraKeywords: []string{"FETCH", "FIRST", "NEXT", "ROWS", "ONLY", "NULLS"},
	FilterClauses: []string{"WHERE"},
}

// trinoKeywordSet is the compiled keyword configuration for the trino dialect.
var trinoKeywordSet = newKeywordSet(trinoKeywords)

// trinoDialect adapts the package-level Analyze (shared token extractor with
// trino keywords) as the "trino" Dialect.
type trinoDialect struct{}

func (trinoDialect) Name() string { return trinoDialectName }

func (trinoDialect) Analyze(sql string) (*AnalysisResult, error) {
	return Analyze(sql)
}

// Register the Trino dialect so parser.For resolves engine "trino".
func init() {
	Register(trinoDialectName, trinoDialect{})
}
