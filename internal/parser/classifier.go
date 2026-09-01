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

// IsBypass returns true when the statement class should bypass Tier 1/2
// checks entirely. These are read-only metadata/administration commands.
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
func IsMutating(class StatementClass) bool {
	switch class {
	case StatementInsert, StatementUpdate, StatementDelete,
		StatementCreate, StatementAlter, StatementDrop, StatementTruncate:
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
