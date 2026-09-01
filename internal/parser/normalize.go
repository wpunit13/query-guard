package parser

import (
	"strings"
	"unicode"
)

// ──────────────────────────────────────────────────────────────────────────────
// SQL Normalizer
// ──────────────────────────────────────────────────────────────────────────────

// normalizeForVitess strips Trino-specific syntax that vitess cannot parse,
// producing an ANSI/MySQL-compatible SQL string for AST analysis.
//
// Currently handles:
//   - Three-part qualified names (catalog.schema.table → schema.table)
//
// This allows vitess to extract WHERE columns and CTE aliases from Trino
// queries while preserving the original SQL for table-blocklist matching.
func normalizeForVitess(sql string) string {
	var out strings.Builder
	out.Grow(len(sql))

	i := 0
	for i < len(sql) {
		// Skip string literals
		if sql[i] == '\'' {
			out.WriteByte(sql[i])
			i++
			for i < len(sql) && sql[i] != '\'' {
				if sql[i] == '\\' {
					out.WriteByte(sql[i])
					i++
				}
				if i < len(sql) {
					out.WriteByte(sql[i])
					i++
				}
			}
			if i < len(sql) {
				out.WriteByte(sql[i])
				i++
			}
			continue
		}

		// Skip identifiers in double quotes or backticks
		if sql[i] == '"' || sql[i] == '`' {
			quote := sql[i]
			out.WriteByte(sql[i])
			i++
			for i < len(sql) && sql[i] != quote {
				out.WriteByte(sql[i])
				i++
			}
			if i < len(sql) {
				out.WriteByte(sql[i])
				i++
			}
			continue
		}

		// Skip single-line comments
		if i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-' {
			out.WriteString("--")
			i += 2
			for i < len(sql) && sql[i] != '\n' {
				out.WriteByte(sql[i])
				i++
			}
			continue
		}

		// Skip block comments
		if i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*' {
			out.WriteString("/*")
			i += 2
			for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
				out.WriteByte(sql[i])
				i++
			}
			if i+1 < len(sql) {
				out.WriteString("*/")
				i += 2
			}
			continue
		}

		// Check for dot-separated identifiers: look for pattern
		// <identifier>.<identifier>.<identifier> where the first is a
		// catalog name. We strip the first <identifier>. prefix.
		if isIdentStart(sql[i]) {
			start := i
			for i < len(sql) && (isIdentPart(sql[i]) || sql[i] == '.') {
				i++
			}
			segment := sql[start:i]

			// Count dots to detect 3-part qualified names
			dotCount := strings.Count(segment, ".")
			if dotCount >= 2 {
				// Strip the first component (catalog name)
				firstDot := strings.IndexByte(segment, '.')
				stripped := segment[firstDot+1:]
				out.WriteString(stripped)
			} else {
				out.WriteString(segment)
			}
			continue
		}

		out.WriteByte(sql[i])
		i++
	}

	return out.String()
}

// isIdentStart returns true if the byte can start a SQL identifier.
func isIdentStart(b byte) bool {
	return unicode.IsLetter(rune(b)) || b == '_'
}

// isIdentPart returns true if the byte can appear in a SQL identifier.
func isIdentPart(b byte) bool {
	return unicode.IsLetter(rune(b)) || unicode.IsDigit(rune(b)) || b == '_' || b == '$' || b == '.'
}