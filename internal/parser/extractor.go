package parser

import "strings"

// ──────────────────────────────────────────────────────────────────────────────
// Shared token-based extractor
// ──────────────────────────────────────────────────────────────────────────────

// DialectKeywords carries the per-dialect reserved words that steer the shared
// token extractor, so dialect-specific clause words (ClickHouse FINAL,
// PREWHERE, SETTINGS, ...) are never misread as tables, aliases, columns, or
// function names.
type DialectKeywords struct {
	// ExtraKeywords lists dialect-specific reserved words on top of the shared
	// ANSI list (isKeyword in tables.go).
	ExtraKeywords []string

	// ExtraBoundaries lists ExtraKeywords that additionally terminate a
	// predicate scan — clause words that introduce their own assignments
	// (SETTINGS max_threads = 1, FORMAT JSON) whose identifiers must not leak
	// into the WHERE-column set.
	ExtraBoundaries []string

	// FilterClauses lists the keywords that begin a predicate section scanned
	// for column references. WHERE is always scanned; dialects add their own
	// (ClickHouse PREWHERE). JOIN conditions (ON) are scanned universally.
	FilterClauses []string
}

// keywordSet is the compiled form of DialectKeywords used by the walker.
type keywordSet struct {
	extras     map[string]bool
	filters    map[string]bool
	boundaries map[string]bool
}

// predicateBoundaries are the ANSI clause keywords that end a predicate scan.
// Everything else in the keyword set (AND, OR, IN, CAST, CASE, GLOBAL, ...)
// keeps the scan alive so compound predicates are fully extracted.
var predicateBoundaries = map[string]bool{
	"FROM": true, "WHERE": true, "GROUP": true, "ORDER": true,
	"LIMIT": true, "OFFSET": true, "HAVING": true, "UNION": true,
	"EXCEPT": true, "INTERSECT": true, "MINUS": true, "WITH": true,
	"SELECT": true, "INTO": true, "VALUES": true, "WINDOW": true,
	"FOR": true, "JOIN": true,
}

// newKeywordSet compiles a DialectKeywords into lookup tables.
func newKeywordSet(kw *DialectKeywords) *keywordSet {
	set := &keywordSet{
		extras:     make(map[string]bool, len(kw.ExtraKeywords)),
		filters:    map[string]bool{"ON": true},
		boundaries: make(map[string]bool, len(predicateBoundaries)+len(kw.ExtraBoundaries)),
	}
	for _, w := range kw.ExtraKeywords {
		set.extras[strings.ToUpper(w)] = true
	}
	for _, w := range kw.FilterClauses {
		set.filters[strings.ToUpper(w)] = true
	}
	for w := range predicateBoundaries {
		set.boundaries[w] = true
	}
	for _, w := range kw.ExtraBoundaries {
		set.boundaries[strings.ToUpper(w)] = true
	}
	return set
}

// isReserved reports whether an uppercased word is reserved for the dialect.
func (k *keywordSet) isReserved(up string) bool {
	return isKeyword(up) || k.extras[up]
}

// analyzeWithDialect classifies the statement and, for non-bypass verbs, runs
// the shared token extraction with the given dialect keywords.
//
// Executing EXPLAIN forms (EXPLAIN ANALYZE / TYPE EXECUTE) are classified as
// EXPLAIN but run their inner statement, so they are extracted like ordinary
// statements — otherwise their tables, functions, and stars would be invisible
// to Tier 1.
//
// Fail-open: it never returns an error — statements the extractor cannot fully
// understand still yield partial results (e.g. raw table extraction only).
func analyzeWithDialect(sql string, kw *keywordSet) *AnalysisResult {
	class := Classify(sql)
	result := &AnalysisResult{
		StatementClass: class,
		TableAliases:   make(map[string]string),
	}
	if IsBypass(class) {
		if _, executing := SplitExecutingExplain(sql); !executing {
			return result
		}
	}
	extractShared(sql, kw, result)
	return result
}

// extractShared produces the AnalysisResult fields from the raw token stream.
func extractShared(sql string, kw *keywordSet, result *AnalysisResult) {
	tokens := tokenizeRaw(sql)

	// CTE aliases must be collected first so the table scanner can skip them.
	result.CTEAliases = collectCTEAliasesRaw(tokens)
	result.Tables, result.TableFunctions = extractTableNamesRaw(sql, result.CTEAliases)

	walkTokens(tokens, kw, result)
}

// ──────────────────────────────────────────────────────────────────────────────
// CTE alias collection
// ──────────────────────────────────────────────────────────────────────────────

// collectCTEAliasesRaw scans for `WITH [RECURSIVE] name AS (` patterns at any
// nesting depth. Scalar WITH aliases (ClickHouse `WITH 1 AS x`) and expression
// aliases (`WITH f(x) AS y`) do not match the pattern and are not CTEs.
func collectCTEAliasesRaw(tokens []string) []string {
	var aliases []string
	seen := make(map[string]bool)
	for i := 0; i < len(tokens); i++ {
		if !strings.EqualFold(tokens[i], "WITH") {
			continue
		}
		j := i + 1
		if j < len(tokens) && strings.EqualFold(tokens[j], "RECURSIVE") {
			j++
		}
		if j+2 >= len(tokens) {
			continue
		}
		name := tokens[j]
		if !isIdentToken(name) || isKeyword(strings.ToUpper(name)) {
			continue
		}
		if !strings.EqualFold(tokens[j+1], "AS") || tokens[j+2] != "(" {
			continue
		}
		key := strings.ToUpper(name)
		if !seen[key] {
			seen[key] = true
			aliases = append(aliases, name)
		}
	}
	return aliases
}

// ──────────────────────────────────────────────────────────────────────────────
// Token walk — aliases, predicates, functions, projection stars
// ──────────────────────────────────────────────────────────────────────────────

// walkTokens makes a single linear pass over the token stream collecting table
// aliases, WHERE/ON (and dialect filter clause) column references, function
// names, and projection stars.
//
// Predicate subqueries (IN (SELECT ...), EXISTS (SELECT ...)) are WALKED, not
// skipped: stars and functions inside them must be visible to the SELECT-star
// and function blocklists, and their WHERE columns must be recorded — but
// attributed to the subquery's own FROM table, so a name collision with an
// outer table's required-filter column cannot satisfy the filter.
func walkTokens(tokens []string, kw *keywordSet, result *AnalysisResult) {
	depth := 0
	// projStack holds the depths of SELECTs whose projection list is open.
	// A star is a projection star when it appears at the depth of the
	// innermost open SELECT — COUNT(*) and multiplications never match.
	projStack := []int{}
	// inPredicate is true while scanning a WHERE/ON/PREWHERE section.
	inPredicate := false
	// expectTable is true after FROM/JOIN/INTO/TABLE/UPDATE.
	expectTable := false
	// pendingAlias is true after a table reference, waiting for a possible
	// alias token (with or without AS).
	pendingAlias := false
	pendingTable := ""
	// subqueryCtx tracks a predicate subquery being walked. closeDepth is the
	// paren depth its closing ")" restores; fromTable is the subquery's own
	// FROM table, used to attribute unqualified WHERE columns inside it.
	type subqueryCtx struct {
		closeDepth int
		fromTable  string
	}
	var subqueries []subqueryCtx

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]

		switch tok {
		case "(":
			// A parenthesized subquery inside a predicate is walked with its
			// own context (see walkTokens doc).
			if inPredicate && i+1 < len(tokens) && isSubqueryStart(tokens[i+1]) {
				subqueries = append(subqueries, subqueryCtx{closeDepth: depth})
				inPredicate = false
				depth++
				continue
			}
			depth++
			continue
		case ")":
			if depth > 0 {
				depth--
			}
			if n := len(projStack); n > 0 && projStack[n-1] > depth {
				projStack = projStack[:n-1]
			}
			if n := len(subqueries); n > 0 && subqueries[n-1].closeDepth == depth {
				subqueries = subqueries[:n-1]
				inPredicate = true // resume the outer predicate scan
			}
			continue
		case "*":
			expectTable = false
			pendingAlias = false
			if n := len(projStack); n > 0 && projStack[n-1] == depth && isProjectionStarPos(tokens, i) {
				result.SelectAll = true
			}
			continue
		}

		up := strings.ToUpper(tok)
		reserved := kw.isReserved(up)

		// Keywords steer the state machine.
		if reserved {
			expectTable = false
			switch {
			case kw.filters[up]:
				// (Re)enter a predicate section (WHERE, PREWHERE, ON).
				inPredicate = true
				pendingAlias = false
			case kw.boundaries[up]:
				inPredicate = false
				pendingAlias = false
				switch up {
				case "FROM", "INTO", "TABLE", "UPDATE", "JOIN":
					expectTable = true
				case "SELECT":
					projStack = append(projStack, depth)
				}
				if up == "FROM" {
					if n := len(projStack); n > 0 && projStack[n-1] == depth {
						projStack = projStack[:n-1]
					}
				}
			default:
				// Connector (AND, OR, IN, CAST, CASE, GLOBAL, ...): keeps
				// the current mode. AS introduces a table alias.
				if up != "AS" {
					pendingAlias = false
				}
			}
			continue
		}

		if !isIdentToken(tok) {
			// Numbers, strings, operators — irrelevant; they also end any
			// pending table/alias expectation.
			expectTable = false
			pendingAlias = false
			continue
		}

		// Table position: the identifier after FROM/JOIN/INTO/TABLE/UPDATE.
		if expectTable {
			expectTable = false
			pendingTable = tok
			pendingAlias = true
			if n := len(subqueries); n > 0 {
				subqueries[n-1].fromTable = bareName(tok)
			}
			continue
		}

		// Partially-quoted table: the tokenizer split `tpch."lineitem"` into
		// `tpch.` + `"lineitem"`; merge the tail before any alias check.
		if pendingAlias && strings.HasSuffix(pendingTable, ".") {
			pendingTable += tok
			if n := len(subqueries); n > 0 {
				subqueries[n-1].fromTable = bareName(pendingTable)
			}
			continue
		}

		// Alias directly after a table reference (with or without AS).
		if pendingAlias {
			pendingAlias = false
			result.TableAliases[stripIdentQuotes(tok)] = bareName(pendingTable)
			continue
		}

		// Function call: identifier immediately followed by "(".
		if i+1 < len(tokens) && tokens[i+1] == "(" {
			result.Functions = append(result.Functions, strings.ToLower(stripIdentQuotes(tok)))
			continue
		}

		// Column reference inside a predicate section.
		if inPredicate {
			// Unqualified references inside a walked predicate subquery are
			// attributed to the subquery's own FROM table so they cannot
			// satisfy an outer table's required filter by name collision.
			defaultTable := ""
			if n := len(subqueries); n > 0 {
				defaultTable = subqueries[n-1].fromTable
			}
			// Merge qualifier/column tokens the tokenizer split apart
			// (quoted or spaced: `"o"."cid"`, `o . cid`).
			if i+2 < len(tokens) && tokens[i+1] == "." && isIdentToken(tokens[i+2]) {
				recordColumnRef(stripIdentQuotes(tok)+"."+stripIdentQuotes(tokens[i+2]), defaultTable, result)
				i += 2
				continue
			}
			recordColumnRef(tok, defaultTable, result)
		}
	}
}

// isProjectionStarPos reports whether the "*" at index i is a projection-level
// star: directly after SELECT/DISTINCT/ALL, after a comma, or a qualified
// star (`t.*`). A "*" after a plain identifier is multiplication
// (`SELECT price * 2`), and a "*" inside parens is an aggregate argument
// (`COUNT(*)`) — neither is flagged.
func isProjectionStarPos(tokens []string, i int) bool {
	if i == 0 {
		return false
	}
	prev := tokens[i-1]
	if strings.HasSuffix(prev, ".") {
		return true // qualified star: t.*
	}
	switch strings.ToUpper(prev) {
	case "SELECT", "DISTINCT", "ALL", ",":
		return true
	}
	return false
}

// isSubqueryStart reports whether a token begins a subquery body.
func isSubqueryStart(tok string) bool {
	return strings.EqualFold(tok, "SELECT") || strings.EqualFold(tok, "WITH")
}

// skipParenBody returns the index of the ")" closing the "(" at index open,
// tracking nested parentheses. On unbalanced input it returns the last index
// (fail-open: the walk simply ends).
func skipParenBody(tokens []string, open int) int {
	depth := 0
	for j := open; j < len(tokens); j++ {
		switch tokens[j] {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return len(tokens) - 1
}

// recordColumnRef records a column reference from a predicate section,
// splitting `qualifier.column` on the last dot. defaultTable attributes
// unqualified references to a table chosen by the caller (the enclosing
// predicate subquery's FROM table), or "" for the statement level.
func recordColumnRef(tok string, defaultTable string, result *AnalysisResult) {
	column := stripIdentQuotes(tok)
	ref := WhereRef{Column: column}
	if idx := strings.LastIndexByte(column, '.'); idx >= 0 {
		ref.Table = column[:idx]
		ref.Column = column[idx+1:]
	} else if defaultTable != "" {
		ref.Table = defaultTable
	}
	result.WhereColumns = append(result.WhereColumns, ref.Column)
	result.WhereRefs = append(result.WhereRefs, ref)
}

// isIdentToken reports whether a token is an identifier (plain, dotted, or
// quoted). Numbers, strings, and punctuation are not.
func isIdentToken(tok string) bool {
	if tok == "" {
		return false
	}
	c := tok[0]
	return isIdentStart(c) || c == '`' || c == '"'
}

// stripIdentQuotes removes surrounding identifier quotes (backticks or double
// quotes) from a token.
func stripIdentQuotes(tok string) string {
	if len(tok) >= 2 && (tok[0] == '`' || tok[0] == '"') && tok[len(tok)-1] == tok[0] {
		return tok[1 : len(tok)-1]
	}
	return tok
}

// bareName returns the rightmost dot-separated component of a (possibly
// quoted) table token — the bare table name form recorded in TableAliases.
func bareName(table string) string {
	table = stripIdentQuotes(table)
	if idx := strings.LastIndexByte(table, '.'); idx >= 0 {
		table = table[idx+1:]
	}
	return stripIdentQuotes(table)
}
