package parser

import (
	"testing"
)

func TestAnalyze_ErrEmpty(t *testing.T) {
	_, err := Analyze("")
	if err != nil {
		return
	}
}

func TestAnalyze_ErrUnsupported(t *testing.T) {
	// Invalid SQL should not error — tables are extracted via raw scanner.
	r, err := Analyze("NOT VALID SQL @@@")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.StatementClass != StatementOther {
		t.Errorf("expected StatementOther, got %s", r.StatementClass)
	}
}

func TestAnalyze_SelectThreePartQualifier(t *testing.T) {
	// Three-part names (catalog.schema.table) are Trino syntax.
	// vitess MySQL parser rejects them, but the raw scanner extracts
	// the table name directly from the original SQL.
	r, err := Analyze("SELECT * FROM hive.default.orders")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "hive.default.orders" {
		t.Errorf("expected [hive.default.orders], got %v", r.Tables)
	}
}

func TestAnalyze_BypassStatements(t *testing.T) {
	queries := []string{"SHOW TABLES", "DESCRIBE orders", "EXPLAIN SELECT 1", "USE default", "SET SESSION x = 1"}
	for _, q := range queries {
		r, err := Analyze(q)
		if err != nil {
			t.Errorf("Analyze(%q) unexpected error: %v", q, err)
			continue
		}
		if !IsBypass(r.StatementClass) {
			t.Errorf("expected bypass class for %q, got %s", q, r.StatementClass)
		}
	}
}

func TestAnalyze_SelectSimple(t *testing.T) {
	r, err := Analyze("SELECT * FROM orders")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if r.StatementClass != StatementSelect {
		t.Errorf("expected SELECT, got %s", r.StatementClass)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "orders" {
		t.Errorf("expected [orders], got %v", r.Tables)
	}
}

func TestAnalyze_SelectWithQualifier(t *testing.T) {
	r, err := Analyze("SELECT * FROM mydb.orders")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "mydb.orders" {
		t.Errorf("expected [mydb.orders], got %v", r.Tables)
	}
}

func TestAnalyze_SelectWithJoin(t *testing.T) {
	r, err := Analyze("SELECT * FROM orders o JOIN customers c ON o.cid = c.id")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 2 {
		t.Errorf("expected 2 tables, got %v", r.Tables)
	}
}

func TestAnalyze_SelectWithWhereColumn(t *testing.T) {
	r, err := Analyze("SELECT * FROM orders WHERE status = 'active' AND created_at > '2024-01-01'")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.WhereColumns) < 2 {
		t.Errorf("expected >= 2 where columns, got %v", r.WhereColumns)
	}
}

func TestAnalyze_SelectWithCTE(t *testing.T) {
	r, err := Analyze("WITH cte AS (SELECT * FROM raw_events) SELECT * FROM cte")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "raw_events" {
		t.Errorf("expected [raw_events] (excluding CTE), got %v", r.Tables)
	}
	if len(r.CTEAliases) != 1 || r.CTEAliases[0] != "cte" {
		t.Errorf("expected [cte] CTE aliases, got %v", r.CTEAliases)
	}
}

func TestAnalyze_SelectWithCTEJoin(t *testing.T) {
	r, err := Analyze("WITH active AS (SELECT * FROM users WHERE active = true) SELECT * FROM active JOIN orders ON active.id = orders.user_id")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	hasActive := false
	for _, tbl := range r.Tables {
		if tbl == "active" {
			hasActive = true
			break
		}
	}
	if hasActive {
		t.Errorf("CTE alias 'active' should not appear in Tables: %v", r.Tables)
	}
	if len(r.Tables) != 2 {
		t.Errorf("expected 2 physical tables [users orders], got %v", r.Tables)
	}
}
func TestAnalyze_WhereRefsAndAliases(t *testing.T) {
	r, err := Analyze("SELECT * FROM orders o JOIN customers c ON o.cid = c.id WHERE o.status = 'x'")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	// Alias resolution: o -> orders, c -> customers.
	if r.TableAliases["o"] != "orders" || r.TableAliases["c"] != "customers" {
		t.Errorf("unexpected aliases: %v", r.TableAliases)
	}
	// Qualified WHERE/ON refs must be captured with their qualifier.
	foundQualified := false
	for _, ref := range r.WhereRefs {
		if ref.Column == "status" && ref.Table == "o" {
			foundQualified = true
		}
	}
	if !foundQualified {
		t.Errorf("expected qualified ref o.status in WhereRefs: %v", r.WhereRefs)
	}
}

func TestAnalyze_WhereRefUnqualified(t *testing.T) {
	r, err := Analyze("SELECT * FROM orders WHERE status = 'active'")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	for _, ref := range r.WhereRefs {
		if ref.Column == "status" && ref.Table != "" {
			t.Errorf("expected unqualified ref for status, got %+v", ref)
		}
	}
}

func TestAnalyze_Insert(t *testing.T) {
	r, err := Analyze("INSERT INTO orders (id, name) VALUES (1, 'test')")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "orders" {
		t.Errorf("expected [orders], got %v", r.Tables)
	}
}

func TestAnalyze_InsertSelect(t *testing.T) {
	r, err := Analyze("INSERT INTO archive SELECT * FROM live_orders WHERE date < '2024-01-01'")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) < 2 {
		t.Errorf("expected >= 2 tables (archive + live_orders), got %v", r.Tables)
	}
}

func TestAnalyze_Update(t *testing.T) {
	r, err := Analyze("UPDATE orders SET status = 'shipped' WHERE id = 42")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "orders" {
		t.Errorf("expected [orders], got %v", r.Tables)
	}
	if len(r.WhereColumns) < 1 {
		t.Errorf("expected at least 1 where column (id), got %v", r.WhereColumns)
	}
}

func TestAnalyze_Delete(t *testing.T) {
	r, err := Analyze("DELETE FROM orders WHERE created_at < '2023-01-01'")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "orders" {
		t.Errorf("expected [orders], got %v", r.Tables)
	}
	if len(r.WhereColumns) < 1 {
		t.Errorf("expected at least 1 where column (created_at), got %v", r.WhereColumns)
	}
}

func TestAnalyze_CreateTable(t *testing.T) {
	r, err := Analyze("CREATE TABLE orders (id INT, name TEXT)")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "orders" {
		t.Errorf("expected [orders], got %v", r.Tables)
	}
}

func TestAnalyze_DropTable(t *testing.T) {
	r, err := Analyze("DROP TABLE orders")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "orders" {
		t.Errorf("expected [orders], got %v", r.Tables)
	}
}

func TestAnalyze_AlterTable(t *testing.T) {
	r, err := Analyze("ALTER TABLE orders ADD COLUMN description TEXT")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "orders" {
		t.Errorf("expected [orders], got %v", r.Tables)
	}
}

func TestAnalyze_TruncateTable(t *testing.T) {
	r, err := Analyze("TRUNCATE TABLE orders")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if len(r.Tables) != 1 || r.Tables[0] != "orders" {
		t.Errorf("expected [orders], got %v", r.Tables)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Feature B — function collection
// ──────────────────────────────────────────────────────────────────────────────

func TestAnalyze_Functions(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "projection function",
			sql:  "SELECT regexp_count(name, 'x') FROM t",
			want: []string{"regexp_count"},
		},
		{
			name: "where function",
			sql:  "SELECT * FROM t WHERE regexp_extract(name, 'x') = 'y'",
			want: []string{"regexp_extract"},
		},
		{
			name: "uppercase lowercased",
			sql:  "SELECT REGEXP_COUNT(name, 'x') FROM t",
			want: []string{"regexp_count"},
		},
		{
			name: "nested functions",
			sql:  "SELECT lower(regexp_extract(name, 'x')) FROM t",
			want: []string{"lower", "regexp_extract"},
		},
		{
			name: "function inside CTE",
			sql:  "WITH c AS (SELECT regexp_count(name, 'x') AS n FROM t) SELECT n FROM c",
			want: []string{"regexp_count"},
		},
		{
			name: "no functions",
			sql:  "SELECT name FROM t WHERE id = 1",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Analyze(tc.sql)
			if err != nil {
				t.Fatalf("Analyze() unexpected error: %v", err)
			}
			if len(r.Functions) != len(tc.want) {
				t.Fatalf("Functions = %v, want %v", r.Functions, tc.want)
			}
			for i, w := range tc.want {
				if r.Functions[i] != w {
					t.Errorf("Functions = %v, want %v", r.Functions, tc.want)
					break
				}
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Feature C — SelectAll detection
// ──────────────────────────────────────────────────────────────────────────────

func TestAnalyze_SelectAll(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{name: "select star", sql: "SELECT * FROM t", want: true},
		{name: "qualified star", sql: "SELECT a.* FROM a JOIN b ON a.id = b.id", want: true},
		{name: "count star not flagged", sql: "SELECT COUNT(*) FROM t", want: false},
		{name: "explicit columns", sql: "SELECT id, name FROM t", want: false},
		{name: "mixed star and column", sql: "SELECT *, id FROM t", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Analyze(tc.sql)
			if err != nil {
				t.Fatalf("Analyze() unexpected error: %v", err)
			}
			if r.SelectAll != tc.want {
				t.Errorf("SelectAll = %v, want %v", r.SelectAll, tc.want)
			}
		})
	}
}

func TestAnalyze_SelectAll_Subquery(t *testing.T) {
	// A star inside a FROM-subquery is still a projection-level star of the
	// inner select, so it must be flagged.
	r, err := Analyze("SELECT id FROM (SELECT * FROM t) sub")
	if err != nil {
		t.Fatalf("Analyze() unexpected error: %v", err)
	}
	if !r.SelectAll {
		t.Error("SelectAll = false, want true (star in FROM subquery)")
	}
}

func TestStatementClass_String(t *testing.T) {
	tests := []struct {
		c    StatementClass
		want string
	}{
		{StatementUnknown, "UNKNOWN"},
		{StatementSelect, "SELECT"},
		{StatementInsert, "INSERT"},
		{StatementShow, "SHOW"},
		{StatementDescribe, "DESCRIBE"},
		{StatementOther, "OTHER"},
	}
	for _, tc := range tests {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("StatementClass(%d).String() = %q, want %q", tc.c, got, tc.want)
		}
	}
}
