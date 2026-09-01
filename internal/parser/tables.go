package parser

import (
	"strings"
)

// ──────────────────────────────────────────────────────────────────────────────
// Raw Table Name Extractor
// ──────────────────────────────────────────────────────────────────────────────

// extractTableNamesRaw scans a SQL string and extracts fully-qualified table
// names following FROM, JOIN, INTO, TABLE, and UPDATE keywords.
//
// This operates on the ORIGINAL SQL (before normalization) so that 3-part
// catalog.schema.table names are preserved for blocklist matching.
// skipAliases is a list of CTE alias names to exclude from the result.
func extractTableNamesRaw(sql string, skipAliases []string) []string {
	var tables []string
	seen := make(map[string]bool)

	// Build a set of CTE aliases to skip
	skipSet := make(map[string]bool)
	for _, a := range skipAliases {
		skipSet[strings.ToUpper(a)] = true
	}

	tokens := tokenizeRaw(sql)

	for i := 0; i < len(tokens); i++ {
		upper := strings.ToUpper(tokens[i])

		switch {
		case upper == "FROM" || upper == "INTO" || upper == "TABLE" || upper == "UPDATE":
			// Next non-keyword, non-punctuation token should be a table name
			for j := i + 1; j < len(tokens); j++ {
				next := tokens[j]
				if next == "," || next == "(" || next == ")" || next == ";" {
					break
				}
				if next == "" || isKeyword(strings.ToUpper(next)) {
					break
				}
				// Skip if this name matches a CTE alias
				if skipSet[strings.ToUpper(next)] {
					break
				}
				if !seen[next] {
					seen[next] = true
					tables = append(tables, next)
				}
				break
			}

		case isJoinPrefix(upper):
			joinIdx := findJoinIdx(tokens, i)
			if joinIdx > 0 && joinIdx < len(tokens) {
				next := tokens[joinIdx]
				if !isKeyword(strings.ToUpper(next)) && !skipSet[strings.ToUpper(next)] && !seen[next] {
					seen[next] = true
					tables = append(tables, next)
				}
			}
		}
	}

	return tables
}

// isJoinPrefix returns true if the token starts a JOIN clause variant.
func isJoinPrefix(upper string) bool {
	switch upper {
	case "JOIN", "LEFT", "RIGHT", "FULL", "INNER", "CROSS", "NATURAL", "LATERAL":
		return true
	}
	return false
}

// findJoinIdx returns the index of the table name token after a JOIN prefix.
func findJoinIdx(tokens []string, i int) int {
	upper := strings.ToUpper(tokens[i])
	switch upper {
	case "JOIN", "LATERAL":
		return i + 1
	case "LEFT", "RIGHT", "FULL":
		if i+2 < len(tokens) && strings.ToUpper(tokens[i+2]) == "JOIN" {
			if i+1 < len(tokens) && strings.ToUpper(tokens[i+1]) == "OUTER" {
				return i + 3
			}
			return i + 2
		}
	case "INNER", "CROSS", "NATURAL":
		if i+1 < len(tokens) && strings.ToUpper(tokens[i+1]) == "JOIN" {
			return i + 2
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
		"START", "TRANSACTION", "SESSION", "UNNEST":
		return true
	}
	return false
}