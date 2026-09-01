package parser

import (
	"vitess.io/vitess/go/vt/sqlparser"
)

// ──────────────────────────────────────────────────────────────────────────────
// AST Analysis result types
// ──────────────────────────────────────────────────────────────────────────────

// AnalysisResult holds all information extracted from a SQL statement.
//
// Tables are populated from the ORIGINAL SQL via a lightweight token scanner,
// so 3-part qualified names (catalog.schema.table) are preserved for policy
// matching. WhereColumns and CTEAliases are extracted from the normalized SQL
// via vitess/sqlparser after stripping Trino-specific syntax.
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
// references, and CTE aliases. Table names are extracted from the original
// SQL via a lightweight token scanner (preserving 3-part Trino names).
// WHERE columns and CTE aliases come from vitess/sqlparser after stripping
// Trino-specific syntax via normalization. If parsing fails, tables are
// still returned safely (fail-open design).
func Analyze(sql string) (*AnalysisResult, error) {
	class := Classify(sql)

	result := &AnalysisResult{
		StatementClass: class,
		TableAliases:   make(map[string]string),
	}

	// Bypass statements don't need any analysis.
	if IsBypass(class) {
		return result, nil
	}

	// Normalize the SQL for vitess (strip Trino-specific syntax like
	// 3-part qualified names) so we can extract WHERE columns and CTE aliases.
	normalized := normalizeForVitess(sql)

	parser := sqlparser.NewTestParser()
	stmt, err := parser.Parse(normalized)
	if err != nil {
		// Fallback: extract what we can from the original SQL via raw scanner.
		result.Tables = extractTableNamesRaw(sql, nil)
		return result, nil
	}

	// Extract CTE aliases first (needed to filter the raw table scanner).
	collectCTEAliases(stmt, result)

	// Extract raw table names from the original SQL, skipping CTE aliases.
	result.Tables = extractTableNamesRaw(sql, result.CTEAliases)

	// Extract WHERE columns and remaining structure from the vitess AST.
	switch s := stmt.(type) {
	case *sqlparser.Select:
		analyzeSelect(s, result)
	case *sqlparser.Insert:
		analyzeInsert(s, result)
	case *sqlparser.Update:
		analyzeUpdate(s, result)
	case *sqlparser.Delete:
		analyzeDelete(s, result)
	case sqlparser.DDLStatement:
		analyzeDDL(s, result)
	}

	return result, nil
}

// collectCTEAliases walks a vitess AST and populates result.CTEAliases
// from any WITH clauses found. This must run before the raw table scanner
// so CTE aliases can be excluded from the table list.
func collectCTEAliases(stmt sqlparser.Statement, result *AnalysisResult) {
	switch s := stmt.(type) {
	case *sqlparser.Select:
		if s.With != nil {
			for _, cte := range s.With.CTEs {
				result.CTEAliases = append(result.CTEAliases, cte.ID.String())
				// Recurse into CTE subquery for nested CTEs
				if ss, ok := cte.Subquery.(*sqlparser.Select); ok {
					collectCTEAliases(ss, result)
				}
			}
		}
		// Also check subqueries in FROM
		for _, te := range s.From {
			if derived, ok := te.(*sqlparser.AliasedTableExpr); ok {
				if sub, ok := derived.Expr.(*sqlparser.DerivedTable); ok {
					if sel, ok := sub.Select.(*sqlparser.Select); ok {
						collectCTEAliases(sel, result)
					}
				}
			}
		}
	case *sqlparser.Insert:
		if sel, ok := s.Rows.(*sqlparser.Select); ok {
			collectCTEAliases(sel, result)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// SELECT analysis (columns and CTEs only — tables from raw scanner)
// ──────────────────────────────────────────────────────────────────────────────

func analyzeSelect(sel *sqlparser.Select, result *AnalysisResult) {
	// CTE aliases already collected by collectCTEAliases.
	// Here we just recurse into CTE subqueries for WHERE columns.

	// 1. Recurse into CTE subqueries for column references.
	if sel.With != nil {
		for _, cte := range sel.With.CTEs {
			if ss, ok := cte.Subquery.(*sqlparser.Select); ok {
				analyzeSelect(ss, result)
			}
		}
	}

	// 2. Extract WHERE column references.
	if sel.Where != nil {
		extractColumnsFromExpr(sel.Where.Expr, result)
	}

	// 3. Collect table aliases (needed to resolve qualified column refs).
	for _, tableExpr := range sel.From {
		collectTableAliases(tableExpr, result)
	}

	// 4. Walk FROM/JOIN for ON-clause column references only (not table names).
	for _, tableExpr := range sel.From {
		extractOnClauseColumns(tableExpr, result)
	}
}

// collectTableAliases walks a FROM/JOIN table expression and records aliases
// (alias -> physical table bare name) into result.TableAliases.
func collectTableAliases(expr sqlparser.TableExpr, result *AnalysisResult) {
	switch e := expr.(type) {
	case *sqlparser.AliasedTableExpr:
		if !e.As.IsEmpty() {
			if tn, ok := e.Expr.(sqlparser.TableName); ok {
				result.TableAliases[e.As.String()] = tn.Name.String()
			}
		}
	case *sqlparser.JoinTableExpr:
		collectTableAliases(e.LeftExpr, result)
		collectTableAliases(e.RightExpr, result)
	case *sqlparser.ParenTableExpr:
		for _, inner := range e.Exprs {
			collectTableAliases(inner, result)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// INSERT / UPDATE / DELETE analysis
// ──────────────────────────────────────────────────────────────────────────────

func analyzeInsert(ins *sqlparser.Insert, result *AnalysisResult) {
	// Tables already extracted by the raw scanner.

	// INSERT ... SELECT may have a subquery with columns and CTEs.
	if sel, ok := ins.Rows.(*sqlparser.Select); ok {
		analyzeSelect(sel, result)
	}
}

func analyzeUpdate(upd *sqlparser.Update, result *AnalysisResult) {
	// Tables already extracted by the raw scanner.
	if upd.Where != nil {
		extractColumnsFromExpr(upd.Where.Expr, result)
	}
	for _, tableExpr := range upd.TableExprs {
		extractOnClauseColumns(tableExpr, result)
	}
}

func analyzeDelete(del *sqlparser.Delete, result *AnalysisResult) {
	// Tables already extracted by the raw scanner.
	if del.Where != nil {
		extractColumnsFromExpr(del.Where.Expr, result)
	}
}

func analyzeDDL(ddl sqlparser.DDLStatement, result *AnalysisResult) {
	// Tables already extracted by the raw scanner.
	_ = ddl
}

// ──────────────────────────────────────────────────────────────────────────────
// ON-clause column extraction (table-agnostic)
// ──────────────────────────────────────────────────────────────────────────────

// extractOnClauseColumns walks a TableExpr looking only for ON clause column
// references in JOIN conditions. It does NOT extract table names — those are
// handled by the raw token scanner.
func extractOnClauseColumns(expr sqlparser.TableExpr, result *AnalysisResult) {
	switch e := expr.(type) {
	case *sqlparser.JoinTableExpr:
		if e.Condition != nil && e.Condition.On != nil {
			extractColumnsFromExpr(e.Condition.On, result)
		}
		extractOnClauseColumns(e.LeftExpr, result)
		extractOnClauseColumns(e.RightExpr, result)
	case *sqlparser.ParenTableExpr:
		for _, inner := range e.Exprs {
			extractOnClauseColumns(inner, result)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Column extraction helpers
// ──────────────────────────────────────────────────────────────────────────────

// extractColumnsFromExpr recursively walks an expression tree and collects
// column name references into result.WhereColumns.
func extractColumnsFromExpr(expr sqlparser.Expr, result *AnalysisResult) {
	if expr == nil {
		return
	}

	switch e := expr.(type) {
	case *sqlparser.ColName:
		result.WhereColumns = append(result.WhereColumns, e.Name.String())
		ref := WhereRef{Column: e.Name.String()}
		if !e.Qualifier.IsEmpty() {
			ref.Table = e.Qualifier.Name.String()
		}
		result.WhereRefs = append(result.WhereRefs, ref)

	case *sqlparser.ComparisonExpr:
		extractColumnsFromExpr(e.Left, result)
		extractColumnsFromExpr(e.Right, result)

	case *sqlparser.AndExpr:
		extractColumnsFromExpr(e.Left, result)
		extractColumnsFromExpr(e.Right, result)

	case *sqlparser.OrExpr:
		extractColumnsFromExpr(e.Left, result)
		extractColumnsFromExpr(e.Right, result)

	case *sqlparser.NotExpr:
		extractColumnsFromExpr(e.Expr, result)

	case *sqlparser.IsExpr:
		extractColumnsFromExpr(e.Left, result)

	case *sqlparser.FuncExpr:
		for _, arg := range e.Exprs {
			extractColumnsFromExpr(arg, result)
		}

	case *sqlparser.BetweenExpr:
		extractColumnsFromExpr(e.Left, result)
		extractColumnsFromExpr(e.From, result)
		extractColumnsFromExpr(e.To, result)

	case *sqlparser.BinaryExpr:
		extractColumnsFromExpr(e.Left, result)
		extractColumnsFromExpr(e.Right, result)

	case *sqlparser.UnaryExpr:
		extractColumnsFromExpr(e.Expr, result)
	}
}
