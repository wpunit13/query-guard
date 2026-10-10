package parser

import (
	"strings"
	"unicode"
)

// ──────────────────────────────────────────────────────────────────────────────
// StatementClass — enum for SQL statement categories
// ──────────────────────────────────────────────────────────────────────────────

// StatementClass categorises a SQL statement's primary verb.
type StatementClass int

const (
	StatementUnknown  StatementClass = iota
	StatementSelect                  // SELECT / WITH ... SELECT
	StatementInsert                  // INSERT
	StatementUpdate                  // UPDATE
	StatementDelete                  // DELETE
	StatementCreate                  // CREATE
	StatementAlter                   // ALTER
	StatementDrop                    // DROP
	StatementCall                    // CALL
	StatementShow                    // SHOW          — bypass
	StatementDescribe                // DESCRIBE      — bypass
	StatementExplain                 // EXPLAIN       — bypass
	StatementUse                     // USE           — bypass
	StatementSet                     // SET [SESSION] — bypass
	StatementTruncate                // TRUNCATE
	StatementOther                   // anything else (e.g. GRANT, REVOKE, etc.)

	// ClickHouse verbs.
	StatementOptimize // OPTIMIZE TABLE — mutating
	StatementSystem   // SYSTEM         — administration (deny via statement_blocklist)
	StatementAttach   // ATTACH         — mutating
	StatementDetach   // DETACH         — mutating
	StatementRename   // RENAME         — mutating
	StatementExchange // EXCHANGE       — mutating
	StatementKill     // KILL           — administration (deny via statement_blocklist)
)

// String returns a human-readable name for the class.
func (c StatementClass) String() string {
	switch c {
	case StatementSelect:
		return "SELECT"
	case StatementInsert:
		return "INSERT"
	case StatementUpdate:
		return "UPDATE"
	case StatementDelete:
		return "DELETE"
	case StatementCreate:
		return "CREATE"
	case StatementAlter:
		return "ALTER"
	case StatementDrop:
		return "DROP"
	case StatementCall:
		return "CALL"
	case StatementShow:
		return "SHOW"
	case StatementDescribe:
		return "DESCRIBE"
	case StatementExplain:
		return "EXPLAIN"
	case StatementUse:
		return "USE"
	case StatementSet:
		return "SET"
	case StatementTruncate:
		return "TRUNCATE"
	case StatementOptimize:
		return "OPTIMIZE"
	case StatementSystem:
		return "SYSTEM"
	case StatementAttach:
		return "ATTACH"
	case StatementDetach:
		return "DETACH"
	case StatementRename:
		return "RENAME"
	case StatementExchange:
		return "EXCHANGE"
	case StatementKill:
		return "KILL"
	case StatementOther:
		return "OTHER"
	default:
		return "UNKNOWN"
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Classify — extract the verb from a SQL string
// ──────────────────────────────────────────────────────────────────────────────

// Classify scans the first word of a SQL statement (ignoring leading comments)
// and returns the corresponding StatementClass.
func Classify(sql string) StatementClass {
	verb := extractVerb(sql)
	if verb == "" {
		return StatementUnknown
	}

	// A WITH statement's class is the *outermost* verb after the CTE block,
	// not "WITH" itself. Otherwise `WITH x AS (...) DELETE ...` would be
	// misclassified as SELECT and wrongly pre-flighted as a read.
	if verb == "WITH" {
		return classifyWith(sql)
	}

	if c, ok := verbClass(verb); ok {
		return c
	}
	return StatementOther
}

// verbClass maps a statement verb to its class.
func verbClass(verb string) (StatementClass, bool) {
	switch verb {
	case "SELECT":
		return StatementSelect, true
	case "INSERT":
		return StatementInsert, true
	case "UPDATE":
		return StatementUpdate, true
	case "DELETE":
		return StatementDelete, true
	case "CREATE":
		return StatementCreate, true
	case "ALTER":
		return StatementAlter, true
	case "DROP":
		return StatementDrop, true
	case "CALL":
		return StatementCall, true
	case "SHOW":
		return StatementShow, true
	case "DESCRIBE", "DESC":
		return StatementDescribe, true
	case "EXPLAIN":
		return StatementExplain, true
	case "USE":
		return StatementUse, true
	case "SET":
		return StatementSet, true
	case "TRUNCATE":
		return StatementTruncate, true
	case "OPTIMIZE":
		return StatementOptimize, true
	case "SYSTEM":
		return StatementSystem, true
	case "ATTACH":
		return StatementAttach, true
	case "DETACH":
		return StatementDetach, true
	case "RENAME":
		return StatementRename, true
	case "EXCHANGE":
		return StatementExchange, true
	case "KILL":
		return StatementKill, true
	default:
		return StatementUnknown, false
	}
}

// classifyWith finds the outermost statement verb that follows the WITH CTE
// definitions. It tokenizes the SQL and returns the first top-level (paren
// depth 0) verb after the initial WITH keyword, skipping CTE names and their
// parenthesised subqueries.
func classifyWith(sql string) StatementClass {
	tokens := tokenizeRaw(sql)
	depth := 0
	for i, tok := range tokens {
		up := strings.ToUpper(tok)
		switch up {
		case "(":
			depth++
			continue
		case ")":
			depth--
			continue
		}
		if depth > 0 {
			continue
		}
		if i == 0 {
			continue // the WITH keyword itself
		}
		if c, ok := verbClass(up); ok {
			return c
		}
	}
	// No explicit verb found; assume SELECT (a bare WITH ... SELECT).
	return StatementSelect
}

// ─────────────────────────────────────────────────────────────────────────────
// Executing EXPLAIN forms
// ─────────────────────────────────────────────────────────────────────────────

// SplitExecutingExplain reports whether sql is an EXPLAIN form that EXECUTES
// its inner statement on the engine — `EXPLAIN ANALYZE [VERBOSE] ...` and
// `EXPLAIN (... TYPE EXECUTE ...)` — and returns the inner statement with the
// EXPLAIN wrapper stripped. These forms run real queries, so they must not
// bypass Tier 1 the way planning-only EXPLAIN forms do, and Tier 2 must
// pre-flight the inner statement rather than the wrapper.
func SplitExecutingExplain(sql string) (inner string, ok bool) {
	s := strings.TrimSpace(sql)
	if s == "" {
		return "", false
	}
	// Strip leading comments the same way extractVerb does.
	for {
		if strings.HasPrefix(s, "--") {
			idx := strings.Index(s, "\n")
			if idx < 0 {
				return "", false
			}
			s = strings.TrimSpace(s[idx+1:])
			continue
		}
		if strings.HasPrefix(s, "/*") {
			idx := strings.Index(s, "*/")
			if idx < 0 {
				return "", false
			}
			s = strings.TrimSpace(s[idx+2:])
			continue
		}
		break
	}
	if !hasWordPrefix(s, "EXPLAIN") {
		return "", false
	}
	rest := strings.TrimSpace(s[len("EXPLAIN"):])
	if rest == "" {
		return "", false
	}
	if hasWordPrefix(rest, "ANALYZE") {
		rest = strings.TrimSpace(rest[len("ANALYZE"):])
		if hasWordPrefix(rest, "VERBOSE") {
			rest = strings.TrimSpace(rest[len("VERBOSE"):])
		}
		if rest == "" {
			return "", false
		}
		return rest, true
	}
	if strings.HasPrefix(rest, "(") {
		// Option list: find its balanced closing paren, then check for TYPE
		// EXECUTE inside. Anything else (TYPE IO/LOGICAL/DISTRIBUTED/VALIDATE)
		// only plans and stays a bypass.
		depth := 0
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					opts := rest[1:i]
					if !containsWord(opts, "TYPE") || !containsWord(opts, "EXECUTE") {
						return "", false
					}
					inner := strings.TrimSpace(rest[i+1:])
					if inner == "" {
						return "", false
					}
					return inner, true
				}
			}
		}
		return "", false // unbalanced option list
	}
	return "", false
}

// hasWordPrefix reports whether s starts with the word w (case-insensitive)
// followed by a non-identifier byte (or end of string).
func hasWordPrefix(s, w string) bool {
	if len(s) < len(w) || !strings.EqualFold(s[:len(w)], w) {
		return false
	}
	if len(s) == len(w) {
		return true
	}
	return !isIdentPartByte(s[len(w)])
}

// containsWord reports whether the word w appears as a standalone
// (case-insensitive) word in s.
func containsWord(s, w string) bool {
	up := strings.ToUpper(s)
	for i := 0; i+len(w) <= len(up); i++ {
		if up[i:i+len(w)] != w {
			continue
		}
		before := byte(' ')
		if i > 0 {
			before = up[i-1]
		}
		after := byte(' ')
		if i+len(w) < len(up) {
			after = up[i+len(w)]
		}
		if !isIdentPartByte(before) && !isIdentPartByte(after) {
			return true
		}
	}
	return false
}

// isIdentPartByte mirrors the tokenizer's identifier continuation set (without
// the dot, which never follows the words checked here).
func isIdentPartByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '$'
}

// IsBypass returns true when the statement class should bypass Tier 1/2
// checks entirely. These are read-only metadata/administration commands.
//
// ClickHouse SYSTEM and KILL are deliberately NOT bypass classes: they reach
// the server as mutating/administration statements (SYSTEM SHUTDOWN, SYSTEM
// DROP CACHE, KILL QUERY) and must be denied explicitly via
// rules.statement_blocklist when a policy wants them blocked.
func IsBypass(class StatementClass) bool {
	switch class {
	case StatementShow, StatementDescribe, StatementExplain, StatementUse, StatementSet:
		return true
	default:
		return false
	}
}

// IsMutating returns true when the statement class modifies data or schema.
// Such statements bypass cost checks but still go through table blocklist.
// ClickHouse OPTIMIZE (merge/mutation), ATTACH/DETACH (metadata), RENAME and
// EXCHANGE (schema) are mutating.
func IsMutating(class StatementClass) bool {
	switch class {
	case StatementInsert, StatementUpdate, StatementDelete,
		StatementCreate, StatementAlter, StatementDrop, StatementTruncate,
		StatementOptimize, StatementAttach, StatementDetach,
		StatementRename, StatementExchange:
		return true
	default:
		return false
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// extractVerb pulls the first non-comment word out of a SQL string.
// It strips:
//   - Single-line comments   (-- ...)
//   - Multi-line comments    (/* ... */)
//   - Leading whitespace
func extractVerb(sql string) string {
	s := strings.TrimSpace(sql)
	if s == "" {
		return ""
	}

	// Strip leading comments
	for {
		if strings.HasPrefix(s, "--") {
			idx := strings.Index(s, "\n")
			if idx < 0 {
				return ""
			}
			s = strings.TrimSpace(s[idx+1:])
			continue
		}
		if strings.HasPrefix(s, "/*") {
			idx := strings.Index(s, "*/")
			if idx < 0 {
				return ""
			}
			s = strings.TrimSpace(s[idx+2:])
			continue
		}
		break
	}

	// Read the first word (run of non-whitespace characters)
	var word strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) {
			break
		}
		word.WriteRune(unicode.ToUpper(r))
	}
	return word.String()
}
