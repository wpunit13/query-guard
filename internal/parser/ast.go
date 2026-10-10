package parser

// ──────────────────────────────────────────────────────────────────────────────
// AST Analysis result types
// ──────────────────────────────────────────────────────────────────────────────

// AnalysisResult holds all information extracted from a SQL statement.
//
// Tables are populated from the ORIGINAL SQL via a lightweight token scanner,
// so 3-part qualified names (catalog.schema.table) are preserved for policy
// matching. WhereColumns, WhereRefs, functions, and projection stars are
// extracted from the same token stream by the shared dialect-neutral
// extractor (extractor.go), steered by the dialect's keyword configuration.
type AnalysisResult struct {
	// Tables is the set of physical table names referenced (catalog.schema.table
	// or schema.table), excluding CTE aliases.
	Tables []string

	// WhereColumns lists column identifiers that appear in WHERE or ON clauses.
	WhereColumns []string

	// WhereRefs lists WHERE/ON column references with an optional table
	// qualifier (alias or table name). Used for table-aware required-filter
	// enforcement so a predicate on a *different* table cannot satisfy a
	// guarded table's required filter.
	WhereRefs []WhereRef

	// TableAliases maps a table alias to the physical table's bare name.
	TableAliases map[string]string

	// CTEAliases lists the alias names defined in WITH clauses.
	CTEAliases []string

	// Functions lists function/UDF names (lowercased) appearing in the
	// SELECT projections or WHERE/ON clauses, including inside CTEs and
	// subqueries. Used for function-blocklist enforcement.
	Functions []string

	// TableFunctions lists table-valued function names (lowercased) invoked in
	// FROM/JOIN position, e.g. merge('db','regex') or remote('host', db, table).
	// A table function reads rows without naming a physical table, so table
	// blocklists cannot see through it; policies deny them via
	// rules.table_function_blocklist. Each entry is also present in Tables
	// (recorded under the function name) so existing table_blocklist entries
	// keep matching.
	TableFunctions []string

	// SelectAll is true when the statement projects `*` or `table.*` at the
	// top level of a SELECT list. COUNT(*) and other aggregate arguments do
	// NOT set this flag — only projection-level stars do.
	SelectAll bool

	// StatementClass is the classified verb category.
	StatementClass StatementClass
}

// WhereRef is a column reference in a WHERE or ON clause, with the table
// qualifier it was written against (empty when unqualified).
type WhereRef struct {
	// Table is the qualifier (alias or table name), or "" if unqualified.
	Table string
	// Column is the bare column name.
	Column string
}

// ──────────────────────────────────────────────────────────────────────────────
// Analyze — parse and extract metadata from a SQL statement
// ──────────────────────────────────────────────────────────────────────────────

// Analyze parses the SQL string and extracts table names, WHERE column
// references, table aliases, function names, and projection stars. Table
// names are extracted from the original SQL via a lightweight token scanner
// (preserving 3-part Trino names); the rest comes from the shared
// dialect-neutral token extractor with the trino keyword configuration.
//
// Fail-open: parsing never fails — statements the extractor cannot fully
// understand still yield partial results (e.g. raw table extraction only).
func Analyze(sql string) (*AnalysisResult, error) {
	return analyzeWithDialect(sql, trinoKeywordSet), nil
}
