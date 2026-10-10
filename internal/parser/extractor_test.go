package parser

import (
	"reflect"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────────
// Shared token extractor tests
//
// The package-level Analyze routes to the shared extractor with the trino
// keyword configuration, so the existing ast_test.go fixtures double as
// extractor coverage. These tests cover behaviour the fixtures do not.
// ──────────────────────────────────────────────────────────────────────────────

func TestExtract_MultiplicationIsNotProjectionStar(t *testing.T) {
	r, err := Analyze("SELECT price * 2 FROM orders")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if r.SelectAll {
		t.Error("SelectAll = true, want false (multiplication, not a star)")
	}
}

func TestExtract_QualifiedStar(t *testing.T) {
	r, err := Analyze("SELECT o.* FROM orders o")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if !r.SelectAll {
		t.Error("SelectAll = false, want true (qualified star)")
	}
	if r.TableAliases["o"] != "orders" {
		t.Errorf("TableAliases[o] = %q, want orders", r.TableAliases["o"])
	}
}

func TestExtract_ExplicitAliasWithAS(t *testing.T) {
	r, err := Analyze("SELECT * FROM orders AS o WHERE o.status = 'x'")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if r.TableAliases["o"] != "orders" {
		t.Errorf("TableAliases[o] = %q, want orders", r.TableAliases["o"])
	}
	found := false
	for _, ref := range r.WhereRefs {
		if ref.Column == "status" && ref.Table == "o" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected qualified ref o.status, got %v", r.WhereRefs)
	}
}

func TestExtract_FetchFirstSuffixNotColumns(t *testing.T) {
	r, err := Analyze("SELECT id FROM orders WHERE status = 'x' ORDER BY id OFFSET 5 ROWS FETCH FIRST 10 ROWS ONLY")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	for _, col := range r.WhereColumns {
		switch col {
		case "fetch", "first", "rows", "only", "offset":
			t.Errorf("result suffix leaked into WhereColumns: %v", r.WhereColumns)
		}
	}
	if !contains(r.WhereColumns, "status") {
		t.Errorf("expected status in WhereColumns, got %v", r.WhereColumns)
	}
}

func TestExtract_SubqueryInPredicateSkipped(t *testing.T) {
	// Columns of an IN-subquery are not predicate columns of the outer
	// statement (parity with the previous AST path); the subquery's tables
	// are still extracted for blocklist matching.
	r, err := Analyze("SELECT * FROM orders WHERE id IN (SELECT order_id FROM items) AND status = 'x'")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if contains(r.WhereColumns, "order_id") {
		t.Errorf("subquery column leaked into WhereColumns: %v", r.WhereColumns)
	}
	if !contains(r.WhereColumns, "status") {
		t.Errorf("expected status in WhereColumns, got %v", r.WhereColumns)
	}
	if !contains(r.Tables, "orders") || !contains(r.Tables, "items") {
		t.Errorf("expected [orders items] tables, got %v", r.Tables)
	}
}

func TestExtract_GroupByNotWhereColumns(t *testing.T) {
	r, err := Analyze("SELECT region, count(*) FROM orders WHERE status = 'x' GROUP BY region ORDER BY region LIMIT 10")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if contains(r.WhereColumns, "region") {
		t.Errorf("GROUP BY column leaked into WhereColumns: %v", r.WhereColumns)
	}
	if !contains(r.WhereColumns, "status") {
		t.Errorf("expected status in WhereColumns, got %v", r.WhereColumns)
	}
}

func TestExtract_JoinModifierTable(t *testing.T) {
	// Join modifiers (ASOF/SEMI/ANTI/GLOBAL) must not hide the joined table
	// from the raw scanner.
	r, err := Analyze("SELECT * FROM local.t LEFT SEMI JOIN remote.u ON local.t.id = remote.u.id")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if !contains(r.Tables, "remote.u") {
		t.Errorf("expected remote.u in Tables, got %v", r.Tables)
	}
}

func TestExtract_NestedCTEAliases(t *testing.T) {
	r, err := Analyze("WITH a AS (WITH b AS (SELECT 1) SELECT * FROM b) SELECT * FROM a")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if !contains(r.CTEAliases, "a") || !contains(r.CTEAliases, "b") {
		t.Errorf("expected [a b] CTE aliases, got %v", r.CTEAliases)
	}
	if len(r.Tables) != 0 {
		t.Errorf("expected no physical tables, got %v", r.Tables)
	}
}

func TestExtract_ScalarWithNotCTE(t *testing.T) {
	// ClickHouse-style scalar WITH alias must not be treated as a CTE name
	// (and thus must not hide a table of the same name).
	r, err := Analyze("WITH 1 AS one SELECT * FROM one")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if contains(r.CTEAliases, "one") {
		t.Errorf("scalar WITH alias treated as CTE: %v", r.CTEAliases)
	}
	if !contains(r.Tables, "one") {
		t.Errorf("expected [one] in Tables, got %v", r.Tables)
	}
}

func TestExtract_WhereColumnsMatchLegacyShape(t *testing.T) {
	// WhereColumns holds bare names; WhereRefs carries the qualifier.
	r, err := Analyze("SELECT * FROM orders o JOIN customers c ON o.cid = c.id WHERE o.status = 'x' AND region = 'eu'")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	wantCols := []string{"cid", "id", "status", "region"}
	if !reflect.DeepEqual(r.WhereColumns, wantCols) {
		t.Errorf("WhereColumns = %v, want %v", r.WhereColumns, wantCols)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Predicate-subquery walking
// ─────────────────────────────────────────────────────────────────────────────

func TestExtract_StarInsidePredicateSubquery(t *testing.T) {
	// A projection star inside an IN-subquery must set SelectAll so the
	// SELECT-star rule can fire.
	r, err := Analyze("SELECT name FROM nation WHERE nationkey IN (SELECT * FROM customer)")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if !r.SelectAll {
		t.Error("SelectAll = false, want true (star inside IN-subquery)")
	}
}

func TestExtract_FunctionInsidePredicateSubquery(t *testing.T) {
	r, err := Analyze("SELECT name FROM nation WHERE nationkey IN (SELECT regexp_count(name,'a') FROM nation)")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if !contains(r.Functions, "regexp_count") {
		t.Errorf("Functions = %v, want it to contain regexp_count", r.Functions)
	}
}

func TestExtract_SubqueryWhereColumnsAttributedToSubqueryTable(t *testing.T) {
	// Unqualified WHERE columns inside a predicate subquery are attributed to
	// the subquery's own FROM table — they must NOT satisfy an outer table's
	// required filter by name collision.
	r, err := Analyze("SELECT * FROM orders WHERE id IN (SELECT a FROM other WHERE orderdate > X)")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	found := false
	for _, ref := range r.WhereRefs {
		if ref.Column == "orderdate" {
			if ref.Table != "other" {
				t.Errorf("orderdate ref attributed to %q, want other", ref.Table)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("orderdate not recorded in WhereRefs: %v", r.WhereRefs)
	}
}

func TestExtract_FilteredSubquerySatisfiesItsOwnTable(t *testing.T) {
	// The legit shape: the subquery filters the guarded table it reads. The
	// unqualified filter column is attributed to that table, so the required
	// filter is satisfied and the query is allowed.
	r, err := Analyze("SELECT name FROM nation WHERE nationkey IN (SELECT orderkey FROM tpch.tiny.orders WHERE orderdate > DATE '1994-01-01')")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	found := false
	for _, ref := range r.WhereRefs {
		if ref.Column == "orderdate" && ref.Table == "orders" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected orderdate attributed to orders, got %v", r.WhereRefs)
	}
}

func TestExtract_OuterPredicateColumnsUnqualified(t *testing.T) {
	// Columns of the outer predicate (outside any subquery) keep the
	// unqualified form the handler treats as targeting the guarded table.
	r, err := Analyze("SELECT * FROM orders WHERE id IN (SELECT order_id FROM items) AND status = 'x'")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	for _, ref := range r.WhereRefs {
		if ref.Column == "status" && ref.Table != "" {
			t.Errorf("outer status ref qualified to %q, want empty", ref.Table)
		}
		if ref.Column == "id" && ref.Table != "" {
			t.Errorf("outer id ref qualified to %q, want empty", ref.Table)
		}
	}
	if !contains(r.Tables, "orders") || !contains(r.Tables, "items") {
		t.Errorf("expected [orders items] tables, got %v", r.Tables)
	}
}

func TestExtract_NestedPredicateSubqueries(t *testing.T) {
	// Inner subquery columns attribute to the inner table; after it closes,
	// attribution returns to the outer subquery's table.
	r, err := Analyze("SELECT * FROM a WHERE x IN (SELECT b FROM outer_t WHERE y IN (SELECT c FROM inner_t WHERE z > 1))")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	for _, ref := range r.WhereRefs {
		switch ref.Column {
		case "z":
			if ref.Table != "inner_t" {
				t.Errorf("z attributed to %q, want inner_t", ref.Table)
			}
		case "y":
			if ref.Table != "outer_t" {
				t.Errorf("y attributed to %q, want outer_t", ref.Table)
			}
		case "x":
			if ref.Table != "" {
				t.Errorf("x attributed to %q, want empty (outer statement)", ref.Table)
			}
		}
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────────
// Table-scanner hardening tests
// ──────────────────────────────────────────────────────────────────────────────

func TestExtractTableNames_QuotingStyles(t *testing.T) {
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT * FROM \"lineitem\"", "lineitem"},
		{"SELECT * FROM \"tpch\".\"tiny\".\"lineitem\"", "tpch.tiny.lineitem"},
		{"SELECT * FROM tpch.\"lineitem\"", "tpch.lineitem"},
		{"SELECT * FROM `default`.secret_data", "default.secret_data"},
		{"SELECT * FROM \"default\".\"secret_data\"", "default.secret_data"},
		{"SELECT * FROM \"orders\" o WHERE o.id = 1", "orders"},
	}
	for _, tc := range cases {
		got, _ := extractTableNamesRaw(tc.sql, nil)
		if !contains(got, tc.want) {
			t.Errorf("extractTableNamesRaw(%q) = %v, want it to contain %q", tc.sql, got, tc.want)
		}
	}
}

func TestExtractTableNames_CommaList(t *testing.T) {
	got, _ := extractTableNamesRaw("SELECT * FROM a, b, c", nil)
	if len(got) != 3 || !contains(got, "a") || !contains(got, "b") || !contains(got, "c") {
		t.Errorf("comma list = %v, want [a b c]", got)
	}
	// INSERT column lists must not leak in as tables.
	got, _ = extractTableNamesRaw("INSERT INTO t (x, y) VALUES (1, 2)", nil)
	if len(got) != 1 || got[0] != "t" {
		t.Errorf("insert column list = %v, want [t]", got)
	}
	// Aliases between comma-list entries must not hide later tables.
	got, _ = extractTableNamesRaw("SELECT * FROM a x, b AS y, c", nil)
	if len(got) != 3 || !contains(got, "a") || !contains(got, "b") || !contains(got, "c") {
		t.Errorf("comma list with aliases = %v, want [a b c]", got)
	}
	// A derived table in a comma list must not hide later tables.
	got, _ = extractTableNamesRaw("SELECT * FROM (SELECT 1) x, lineitem", nil)
	if len(got) != 1 || got[0] != "lineitem" {
		t.Errorf("comma after derived table = %v, want [lineitem]", got)
	}
}

func TestExtractTableNames_ParenTable(t *testing.T) {
	got, _ := extractTableNamesRaw("SELECT * FROM (lineitem)", nil)
	if len(got) != 1 || got[0] != "lineitem" {
		t.Errorf("paren table = %v, want [lineitem]", got)
	}
	// Derived tables must not double-record.
	got, _ = extractTableNamesRaw("SELECT * FROM (SELECT * FROM lineitem) x", nil)
	if len(got) != 1 || got[0] != "lineitem" {
		t.Errorf("derived table = %v, want [lineitem]", got)
	}
}

func TestExtractTableNames_NestedParens(t *testing.T) {
	// Red-team T2: parenthesized relations at any depth must be extracted.
	for _, sql := range []string{
		"SELECT * FROM ((lineitem))",
		"SELECT * FROM (((lineitem)))",
		"SELECT l.linenumber FROM ((tpch.tiny.lineitem)) l",
		"SELECT * FROM ( ( lineitem ) )",
	} {
		got, _ := extractTableNamesRaw(sql, nil)
		if !contains(got, "lineitem") && !contains(got, "tpch.tiny.lineitem") {
			t.Errorf("extractTableNamesRaw(%q) = %v, want it to contain lineitem", sql, got)
		}
	}
}

func TestExtractTableNames_TableFunctions(t *testing.T) {
	// Red-team C2: table-valued functions are recorded separately.
	tables, funcs := extractTableNamesRaw("SELECT * FROM merge('default','^secret')", nil)
	if !contains(tables, "merge") || !contains(funcs, "merge") {
		t.Errorf("merge() = tables %v funcs %v, want merge in both", tables, funcs)
	}
	tables, funcs = extractTableNamesRaw("SELECT * FROM remote('127.0.0.1:9000', 'default', 'secret_data', 'u', 'p')", nil)
	if !contains(funcs, "remote") {
		t.Errorf("remote() funcs = %v, want [remote]", funcs)
	}
	// String-literal arguments must not leak in as tables.
	if contains(tables, "secret_data") {
		t.Errorf("remote() args leaked into tables: %v", tables)
	}
	tables, funcs = extractTableNamesRaw("SELECT * FROM numbers(100000000)", nil)
	if !contains(funcs, "numbers") {
		t.Errorf("numbers() funcs = %v, want [numbers]", funcs)
	}
	// A JOIN to a table function is detected too.
	_, funcs = extractTableNamesRaw("SELECT * FROM nation JOIN merge('db','x') ON true", nil)
	if !contains(funcs, "merge") {
		t.Errorf("JOIN merge() funcs = %v, want [merge]", funcs)
	}
	// Ordinary tables are never flagged as functions.
	tables, funcs = extractTableNamesRaw("SELECT * FROM orders o WHERE o.id = 1", nil)
	if len(funcs) != 0 {
		t.Errorf("ordinary table flagged as function: %v", funcs)
	}
	if !contains(tables, "orders") {
		t.Errorf("ordinary table missing: %v", tables)
	}
}

func TestExtractTableNames_CommaAfterCTE(t *testing.T) {
	got, _ := extractTableNamesRaw("WITH c AS (SELECT 1) SELECT * FROM c, orders", []string{"c"})
	if len(got) != 1 || got[0] != "orders" {
		t.Errorf("comma after CTE = %v, want [orders]", got)
	}
	// A CTE reference with an alias must not hide later tables either.
	got, _ = extractTableNamesRaw("WITH c AS (SELECT 1) SELECT * FROM c x, orders", []string{"c"})
	if len(got) != 1 || got[0] != "orders" {
		t.Errorf("comma after aliased CTE = %v, want [orders]", got)
	}
}

func TestQualifyBareTables(t *testing.T) {
	got := QualifyBareTables([]string{"orders", "tpch.tiny.lineitem"}, "tpch", "tiny")
	want := []string{"tpch.tiny.orders", "tpch.tiny.lineitem"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("QualifyBareTables = %v, want %v", got, want)
	}
	// Schema-only qualification.
	got = QualifyBareTables([]string{"events"}, "", "default")
	if !reflect.DeepEqual(got, []string{"default.events"}) {
		t.Errorf("schema-only = %v, want [default.events]", got)
	}
	// No schema context: unchanged.
	got = QualifyBareTables([]string{"events"}, "tpch", "")
	if !reflect.DeepEqual(got, []string{"events"}) {
		t.Errorf("catalog-only = %v, want unchanged", got)
	}
}

func TestExtract_RecursiveCTEAliasNotATable(t *testing.T) {
	r, err := Analyze("WITH RECURSIVE n AS (SELECT 1 AS x UNION ALL SELECT x+1 FROM n) SELECT * FROM n")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 0 {
		t.Errorf("Tables = %v, want empty (n is a CTE alias, not a table)", r.Tables)
	}
	if len(r.CTEAliases) != 1 || r.CTEAliases[0] != "n" {
		t.Errorf("CTEAliases = %v, want [n]", r.CTEAliases)
	}
}

func TestExtract_PartQuotedTableAlias(t *testing.T) {
	r, err := Analyze("SELECT x.id FROM tpch.\"lineitem\" x WHERE x.id = 1")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "tpch.lineitem" {
		t.Errorf("Tables = %v, want [tpch.lineitem]", r.Tables)
	}
	if r.TableAliases["x"] != "lineitem" {
		t.Errorf("TableAliases[x] = %q, want lineitem", r.TableAliases["x"])
	}
	if len(r.WhereRefs) != 1 || r.WhereRefs[0].Table != "x" || r.WhereRefs[0].Column != "id" {
		t.Errorf("WhereRefs = %v, want [{x id}]", r.WhereRefs)
	}
}
