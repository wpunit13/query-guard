package parser

// ──────────────────────────────────────────────────────────────────────────────
// ClickHouse dialect — Tier 1 adapter over the shared token extractor
// (target: ClickHouse 26.8 LTS)
// ──────────────────────────────────────────────────────────────────────────────

// clickhouseKeywords lists the ClickHouse clause keywords beyond the shared
// ANSI list: PREWHERE (additional filter clause), FINAL (table modifier after
// FROM), SAMPLE, SETTINGS, FORMAT, GLOBAL (distributed modifiers), plus the
// DDL/engine words that follow table references or introduce assignments
// (ENGINE, TTL, MATERIALIZED, CODEC, DEFAULT, PARTITION, PRIMARY) and compound
// data types that would otherwise be recorded as function calls in DDL
// (Nullable(String), LowCardinality(String), AggregateFunction(...)).
// Join modifiers ASOF/SEMI/ANTI/ANY live in the shared keyword list (tables.go)
// because more than one dialect uses them.
var clickhouseKeywords = &DialectKeywords{
	ExtraKeywords: []string{
		"PREWHERE", "FINAL", "SAMPLE", "SETTINGS", "FORMAT", "GLOBAL",
		"ENGINE", "TTL", "MATERIALIZED", "CODEC", "DEFAULT", "PARTITION",
		"PRIMARY", "APPLY", "WATCH", "SWAP", "EPHEMERAL",
		"NULLABLE", "LOWCARDINALITY", "AGGREGATEFUNCTION",
		"SIMPLEAGGREGATEFUNCTION",
	},
	ExtraBoundaries: []string{"SETTINGS", "FORMAT"},
	FilterClauses:   []string{"WHERE", "PREWHERE"},
}

// clickhouseKeywordSet is the compiled keyword configuration for the
// ClickHouse dialect.
var clickhouseKeywordSet = newKeywordSet(clickhouseKeywords)

// clickhouseDialect implements Dialect for ClickHouse on top of the shared
// token extractor with the ClickHouse keyword list.
//
// Fail-open: Analyze never returns an error — any construct the extractor
// cannot fully understand yields partial results (at minimum the raw table
// extraction), so a query is never rejected because of a parsing gap.
type clickhouseDialect struct{}

func (clickhouseDialect) Name() string { return "clickhouse" }

func (clickhouseDialect) Analyze(sql string) (*AnalysisResult, error) {
	return analyzeWithDialect(sql, clickhouseKeywordSet), nil
}

// Register the ClickHouse dialect so parser.For resolves engine "clickhouse".
func init() {
	Register("clickhouse", clickhouseDialect{})
}
