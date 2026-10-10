package parser

import (
	"strings"
	"unicode"
)

// ──────────────────────────────────────────────────────────────────────────────
// Raw Table Name Extractor
// ──────────────────────────────────────────────────────────────────────────────

// extractTableNamesRaw scans a SQL string and extracts fully-qualified table
// names following FROM, JOIN, INTO, TABLE, and UPDATE keywords.
//
// This operates on the ORIGINAL SQL so that 3-part catalog.schema.table names
// are preserved for blocklist matching. skipAliases is a list of CTE alias
// names to exclude from the result. It handles quoted/partially-quoted
// identifiers, comma-separated table lists, parenthesized relations at any
// paren depth, and table-valued functions, which are returned separately in
// tableFuncs so policies can deny them explicitly.
func extractTableNamesRaw(sql string, skipAliases []string) (tables []string, tableFuncs []string) {
	seen := make(map[string]bool)
	funcsSeen := make(map[string]bool)

	// Build a set of CTE aliases to skip
	skipSet := make(map[string]bool)
	for _, a := range skipAliases {
		skipSet[strings.ToUpper(a)] = true
	}

	// add records a table candidate; isFunc marks a table-valued function
	// invocation, which is recorded both in tableFuncs and (under the function
	// name) in tables so existing table_blocklist entries keep matching.
	add := func(name string, isFunc bool) {
		if isFunc && !funcsSeen[name] {
			funcsSeen[name] = true
			tableFuncs = append(tableFuncs, name)
		}
		if !seen[name] {
			seen[name] = true
			tables = append(tables, name)
		}
	}

	tokens := tokenizeRaw(sql)

	for i := 0; i < len(tokens); i++ {
		upper := strings.ToUpper(tokens[i])

		switch {
		case upper == "FROM" || upper == "INTO" || upper == "TABLE" || upper == "UPDATE":
			extracted := false  // a relation was consumed in this clause
			afterComma := false // the previous token was a list comma
		scan:
			for j := i + 1; j < len(tokens); j++ {
				tok := tokens[j]
				if tok == "," {
					afterComma = true
					continue
				}
				if strings.EqualFold(tok, "AS") && !extracted {
					// Leading AS before the first relation: keep scanning.
					continue
				}
				if tok == "(" {
					// Skip consecutive opening parens: FROM ((lineitem)).
					open := j
					for j+1 < len(tokens) && tokens[j+1] == "(" {
						j++
					}
					switch {
					case j+1 < len(tokens) && isTablePart(tokens[j+1]) && (!extracted || afterComma):
						// Parenthesized relation: FROM (lineitem).
						j++
						tok = tokens[j]
					case j+1 < len(tokens) && isSubqueryStart(tokens[j+1]):
						// Parenthesized subquery: skip its balanced body (and
						// optional alias); a following comma may introduce
						// another relation.
						j = skipParenBody(tokens, open)
						if j+1 < len(tokens) && strings.EqualFold(tokens[j+1], "AS") {
							j++
						}
						if j+1 < len(tokens) && isIdentToken(tokens[j+1]) && !isKeyword(strings.ToUpper(tokens[j+1])) {
							j++
						}
						continue
					default:
						// A paren directly after an extracted table is a column
						// list (INSERT INTO t (a, b)) or something we do not
						// understand — stop scanning this clause.
						break scan
					}
				}
				name, end := tableCandidateAt(tokens, j)
				if name == "" {
					break
				}
				j = end
				// Table-valued function: ident immediately followed by "("
				// in FROM position — skip its argument list so a following
				// alias or comma-list entry is handled correctly.
				isFunc := j+1 < len(tokens) && tokens[j+1] == "("
				if isFunc {
					j = skipParenBody(tokens, j+1)
				}
				// Skip the relation's optional alias (with or without AS) so
				// later comma-list entries are not mistaken for tables.
				if j+1 < len(tokens) && strings.EqualFold(tokens[j+1], "AS") {
					j++
				}
				if j+1 < len(tokens) && isIdentToken(tokens[j+1]) && !isKeyword(strings.ToUpper(tokens[j+1])) {
					j++
				}
				if !skipSet[strings.ToUpper(name)] {
					add(name, isFunc)
				}
				extracted = true
				afterComma = false
				// Continue only when a list comma follows; anything else
				// (WHERE, JOIN, SETTINGS, ...) ends the relation list.
				if !(j+1 < len(tokens) && tokens[j+1] == ",") {
					break
				}
			}

		case isJoinPrefix(upper):
			joinIdx := findJoinIdx(tokens, i)
			if joinIdx > 0 && joinIdx < len(tokens) {
				if name, end := tableCandidateAt(tokens, joinIdx); name != "" &&
					!skipSet[strings.ToUpper(name)] && !seen[name] {
					// Table-valued function in JOIN position.
					isFunc := end+1 < len(tokens) && tokens[end+1] == "("
					add(name, isFunc)
				}
			}
		}
	}

	return tables, tableFuncs
}

// tableCandidateAt returns the table reference starting at token index j and
// the index of its last consumed token, or ("", j) when tokens[j] cannot start
// one. It merges quoted/dotted sequences the tokenizer splits into separate
// tokens ("db"."t", `db`.t, tpch."lineitem") into one dotted name and strips
// identifier quotes, so blocklist matching sees the same name regardless of
// quoting style.
func tableCandidateAt(tokens []string, j int) (string, int) {
	if j >= len(tokens) {
		return "", j
	}
	tok := tokens[j]
	if tok == "" || !isIdentToken(tok) {
		return "", j
	}
	// A reserved word immediately followed by "(" in FROM/JOIN position is a
	// table-valued function whose name collides with a keyword (ClickHouse
	// merge('db', regex)); accept it as a candidate so it can be recorded and
	// denied. Keywords NOT followed by "(" are clause words, never tables.
	funcCall := j+1 < len(tokens) && tokens[j+1] == "("
	if isKeyword(strings.ToUpper(tok)) && !funcCall {
		return "", j
	}
	name := tok
	k := j
	for k+1 < len(tokens) {
		switch {
		case tokens[k+1] == "." && k+2 < len(tokens) && isTablePart(tokens[k+2]):
			name += "." + tokens[k+2]
			k += 2
		case strings.HasSuffix(name, ".") && isTablePart(tokens[k+1]):
			// Partially quoted: tpch."lineitem" tokenizes as "tpch." + "lineitem".
			name += tokens[k+1]
			k++
		default:
			return unquoteTableName(name), k
		}
	}
	return unquoteTableName(name), k
}

// isTablePart reports whether a token can be a component of a table name.
func isTablePart(tok string) bool {
	return isIdentToken(tok) && !isKeyword(strings.ToUpper(tok))
}

// unquoteTableName strips identifier quotes from each dot-separated component
// of a table name so matching is quote-style agnostic.
func unquoteTableName(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = stripIdentQuotes(p)
	}
	return strings.Join(parts, ".")
}

// QualifyBareTables prefixes single-component table references with the
// request's catalog/schema context so required-filter scoping cannot be evaded
// by omitting the qualifier (engines resolve bare names against the session
// catalog/schema or the default database). Qualification is skipped entirely
// when no schema context is available (a catalog alone cannot scope a bare
// name safely).
func QualifyBareTables(tables []string, catalog, schema string) []string {
	if schema == "" {
		return tables
	}
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		if strings.Contains(t, ".") {
			out = append(out, t)
			continue
		}
		qualified := schema + "." + t
		if catalog != "" {
			qualified = catalog + "." + qualified
		}
		out = append(out, qualified)
	}
	return out
}

// isJoinPrefix returns true if the token starts a JOIN clause variant.
func isJoinPrefix(upper string) bool {
	switch upper {
	case "JOIN", "LEFT", "RIGHT", "FULL", "INNER", "CROSS", "NATURAL", "LATERAL",
		"GLOBAL", "ANY", "ALL", "ASOF", "SEMI", "ANTI":
		return true
	}
	return false
}

// isJoinModifier reports whether a word is a join modifier that may sit
// between a join prefix and the JOIN keyword (ClickHouse 26.8 syntax:
// `GLOBAL ANY JOIN`, `LEFT ASOF JOIN`, `LEFT SEMI JOIN`, ...).
func isJoinModifier(upper string) bool {
	switch upper {
	case "GLOBAL", "ANY", "ALL", "ASOF", "SEMI", "ANTI":
		return true
	}
	return false
}

// findJoinIdx returns the index of the table name token after a JOIN prefix.
// It skips the modifier keywords that may sit between the prefix and the JOIN
// keyword itself, and returns 0 for the JOIN inside an ARRAY JOIN clause
// (ClickHouse) — the token after it is an array column, not a table.
func findJoinIdx(tokens []string, i int) int {
	for j := i; j < len(tokens) && j-i <= 4; j++ {
		up := strings.ToUpper(tokens[j])
		switch {
		case up == "JOIN":
			if j > 0 && strings.EqualFold(tokens[j-1], "ARRAY") {
				return 0 // ARRAY JOIN clause — not a table join
			}
			return j + 1
		case up == "OUTER" || isJoinModifier(up):
			continue
		}
	}
	return 0
}

// ──────────────────────────────────────────────────────────────────────────────
// Tokenizer
// ──────────────────────────────────────────────────────────────────────────────

// tokenizeRaw breaks a SQL string into tokens, preserving dotted names
// as single tokens and skipping string literals and comments.
func tokenizeRaw(sql string) []string {
	var tokens []string
	var current strings.Builder

	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}

	i := 0
	for i < len(sql) {
		ch := sql[i]

		// Skip whitespace
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			flush()
			i++
			continue
		}

		// Single-line comment
		if ch == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			flush()
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		}

		// Block comment
		if ch == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			flush()
			i += 2
			for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}

		// Single-quoted string
		if ch == '\'' {
			flush()
			tokens = append(tokens, "'")
			i++
			for i < len(sql) && sql[i] != '\'' {
				if sql[i] == '\\' {
					i++
				}
				i++
			}
			if i < len(sql) {
				i++
			}
			continue
		}

		// Punctuation — individual tokens
		if ch == '(' || ch == ')' || ch == ',' || ch == ';' || ch == '=' ||
			ch == '<' || ch == '>' || ch == '!' || ch == '+' || ch == '-' ||
			ch == '*' || ch == '/' {
			flush()
			tokens = append(tokens, string(ch))
			i++
			continue
		}

		// Backtick or double-quoted identifier
		if ch == '`' || ch == '"' {
			flush()
			current.WriteByte(ch)
			i++
			for i < len(sql) && sql[i] != ch {
				current.WriteByte(sql[i])
				i++
			}
			if i < len(sql) {
				current.WriteByte(sql[i])
				i++
			}
			flush()
			continue
		}

		// Identifier (includes dots for qualified names)
		if isIdentStart(ch) {
			for i < len(sql) && (isIdentPart(sql[i]) || sql[i] == '.') {
				current.WriteByte(sql[i])
				i++
			}
			flush()
			continue
		}

		// Any other character — single char token
		flush()
		tokens = append(tokens, string(ch))
		i++
	}

	flush()
	return tokens
}

// isKeyword returns true for SQL reserved words that would never be table names.
func isKeyword(upper string) bool {
	switch upper {
	case "SELECT", "WHERE", "AND", "OR", "NOT", "IN", "IS", "NULL",
		"AS", "ON", "SET", "VALUES", "GROUP", "ORDER", "BY", "HAVING",
		"LIMIT", "OFFSET", "UNION", "ALL", "DISTINCT", "CASE", "WHEN",
		"THEN", "ELSE", "END", "EXISTS", "BETWEEN", "LIKE", "AT",
		"WITH", "RECURSIVE", "TABLE", "FROM", "INTO", "JOIN", "LEFT",
		"RIGHT", "INNER", "OUTER", "CROSS", "FULL", "NATURAL", "LATERAL",
		"USING", "EXCEPT", "INTERSECT", "MINUS", "FOR", "DESC", "ASC",
		"TRUE", "FALSE", "TIMESTAMP", "DATE", "TIME", "INTERVAL",
		"CAST", "TRY_CAST", "ROW", "ARRAY", "MAP", "STRUCT",
		"CREATE", "ALTER", "DROP", "TRUNCATE", "INSERT", "UPDATE", "DELETE",
		"REPLACE", "MERGE", "CALL", "SHOW", "DESCRIBE", "EXPLAIN",
		"USE", "GRANT", "REVOKE", "COMMIT", "ROLLBACK", "BEGIN",
		"START", "TRANSACTION", "SESSION", "UNNEST",
		"ANY", "ASOF", "SEMI", "ANTI":
		return true
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────────
// Identifier classification
// ──────────────────────────────────────────────────────────────────────────────

// isIdentStart returns true if the byte can start a SQL identifier.
func isIdentStart(b byte) bool {
	return unicode.IsLetter(rune(b)) || b == '_'
}

// isIdentPart returns true if the byte can appear in a SQL identifier.
func isIdentPart(b byte) bool {
	return unicode.IsLetter(rune(b)) || unicode.IsDigit(rune(b)) || b == '_' || b == '$' || b == '.'
}
