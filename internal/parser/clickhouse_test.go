package parser

import "testing"

// ──────────────────────────────────────────────────────────────────────────────
// ClickHouse dialect tests
// ──────────────────────────────────────────────────────────────────────────────

// chAnalyze runs the ClickHouse dialect over a statement.
func chAnalyze(t *testing.T, sql string) *AnalysisResult {
	t.Helper()
	res, err := For("clickhouse").Analyze(sql)
	if err != nil {
		t.Fatalf("clickhouse Analyze(%q) unexpected error: %v", sql, err)
	}
	return res
}

// ── Table blocklist coverage ──

func TestClickHouse_TableBlocklist(t *testing.T) {
	cases := []struct {
		name  string
		sql   string
		table string
	}{
		{name: "simple", sql: "SELECT * FROM analytics.events", table: "analytics.events"},
		{name: "FINAL modifier", sql: "SELECT * FROM analytics.events FINAL WHERE user_id = 1", table: "analytics.events"},
		{name: "SAMPLE clause", sql: "SELECT * FROM analytics.events SAMPLE 0.01", table: "analytics.events"},
		{name: "SETTINGS suffix", sql: "SELECT * FROM analytics.events SETTINGS max_threads = 4", table: "analytics.events"},
		{name: "FORMAT suffix", sql: "SELECT * FROM analytics.events FORMAT JSON", table: "analytics.events"},
		{name: "three-part name", sql: "SELECT * FROM cluster.db.events", table: "cluster.db.events"},
		{name: "INSERT INTO", sql: "INSERT INTO analytics.events VALUES (1)", table: "analytics.events"},
		{name: "OPTIMIZE TABLE", sql: "OPTIMIZE TABLE analytics.events FINAL", table: "analytics.events"},
		{name: "ALTER TABLE mutation", sql: "ALTER TABLE analytics.events DELETE WHERE user_id = 1", table: "analytics.events"},
		{name: "RENAME TABLE", sql: "RENAME TABLE analytics.events TO analytics.events_old", table: "analytics.events"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := chAnalyze(t, tc.sql)
			if !contains(r.Tables, tc.table) {
				t.Errorf("Tables = %v, want it to contain %q", r.Tables, tc.table)
			}
		})
	}
}

func TestClickHouse_GlobalJoinTables(t *testing.T) {
	r := chAnalyze(t, "SELECT * FROM local.hits GLOBAL ANY JOIN remote.users ON local.hits.uid = remote.users.id")
	if !contains(r.Tables, "local.hits") || !contains(r.Tables, "remote.users") {
		t.Errorf("Tables = %v, want both join sides", r.Tables)
	}
}

// ── Required filters (WHERE / PREWHERE columns) ──

func TestClickHouse_RequiredFilters(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		column string
	}{
		{name: "where column", sql: "SELECT * FROM db.events WHERE event_date = '2026-10-09'", column: "event_date"},
		{name: "prewhere column", sql: "SELECT * FROM db.events PREWHERE event_date = '2026-10-09'", column: "event_date"},
		{name: "where and prewhere", sql: "SELECT url FROM db.hits PREWHERE counter_id = 1 WHERE event_date = '2026-10-09'", column: "counter_id"},
		{name: "qualified ref", sql: "SELECT * FROM db.events AS e WHERE e.event_date = '2026-10-09'", column: "event_date"},
		{name: "mutation where", sql: "ALTER TABLE db.events DELETE WHERE event_date = '2026-10-09'", column: "event_date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := chAnalyze(t, tc.sql)
			if !contains(r.WhereColumns, tc.column) {
				t.Errorf("WhereColumns = %v, want it to contain %q", r.WhereColumns, tc.column)
			}
		})
	}
}

func TestClickHouse_RequiredFilter_QualifiedRefAttribution(t *testing.T) {
	r := chAnalyze(t, "SELECT * FROM db.events e JOIN db.users u ON e.uid = u.id WHERE e.event_date = '2026-10-09'")
	found := false
	for _, ref := range r.WhereRefs {
		if ref.Column == "event_date" && ref.Table == "e" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected qualified ref e.event_date, got %v", r.WhereRefs)
	}
	if r.TableAliases["e"] != "events" || r.TableAliases["u"] != "users" {
		t.Errorf("TableAliases = %v, want e->events, u->users", r.TableAliases)
	}
}

// ── Function blocklist coverage ──

func TestClickHouse_FunctionBlocklist(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		function string
	}{
		{name: "projection hash", sql: "SELECT cityHash64(user_id) FROM db.t", function: "cityhash64"},
		{name: "combinator function", sql: "SELECT countIf(x = 1) FROM db.t", function: "countif"},
		{name: "where function", sql: "SELECT * FROM db.t WHERE startsWith(url, 'https') = 1", function: "startswith"},
		{name: "nested functions", sql: "SELECT lower(toString(id)) FROM db.t", function: "lower"},
		{name: "aggregate in CTE", sql: "WITH s AS (SELECT sum(bytes) AS n FROM db.t) SELECT n FROM s", function: "sum"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := chAnalyze(t, tc.sql)
			if !contains(r.Functions, tc.function) {
				t.Errorf("Functions = %v, want it to contain %q", r.Functions, tc.function)
			}
		})
	}
}

// ── SELECT * detection ──

func TestClickHouse_SelectAll(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{name: "star", sql: "SELECT * FROM db.t", want: true},
		{name: "qualified star", sql: "SELECT t.* FROM db.t", want: true},
		{name: "count() not flagged", sql: "SELECT count() FROM db.t", want: false},
		{name: "explicit columns", sql: "SELECT id, name FROM db.t", want: false},
		{name: "star in subquery", sql: "SELECT id FROM (SELECT * FROM db.t) sub", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := chAnalyze(t, tc.sql)
			if r.SelectAll != tc.want {
				t.Errorf("SelectAll = %v, want %v", r.SelectAll, tc.want)
			}
		})
	}
}

// ── ClickHouse clause keywords are not misread ──

func TestClickHouse_ClauseKeywordsNotColumns(t *testing.T) {
	r := chAnalyze(t, "SELECT * FROM db.hits FINAL WHERE counter_id = 1 SETTINGS max_threads = 4 FORMAT JSON")
	for _, col := range r.WhereColumns {
		switch col {
		case "final", "settings", "max_threads", "format", "json":
			t.Errorf("ClickHouse clause word leaked into WhereColumns: %v", r.WhereColumns)
		}
	}
	if !contains(r.WhereColumns, "counter_id") {
		t.Errorf("expected counter_id in WhereColumns, got %v", r.WhereColumns)
	}
}

func TestClickHouse_LimitByNotColumns(t *testing.T) {
	r := chAnalyze(t, "SELECT * FROM db.hits ORDER BY counter_id LIMIT 10 BY region")
	if contains(r.WhereColumns, "region") {
		t.Errorf("LIMIT BY column leaked into WhereColumns: %v", r.WhereColumns)
	}
}

func TestClickHouse_ArrayJoinNotTable(t *testing.T) {
	r := chAnalyze(t, "SELECT * FROM db.events ARRAY JOIN tags AS tag WHERE tag = 'x'")
	if !contains(r.Tables, "db.events") || len(r.Tables) != 1 {
		t.Errorf("Tables = %v, want only [db.events]", r.Tables)
	}
	if !contains(r.WhereColumns, "tag") {
		t.Errorf("expected tag in WhereColumns, got %v", r.WhereColumns)
	}
}

func TestClickHouse_GlobalInSubquery(t *testing.T) {
	r := chAnalyze(t, "SELECT * FROM db.hits WHERE id GLOBAL IN (SELECT id FROM db.users) AND counter_id = 1")
	// The subquery body is skipped for column extraction, but both tables
	// are extracted for blocklisting, and the scan resumes after GLOBAL IN.
	if !contains(r.Tables, "db.hits") || !contains(r.Tables, "db.users") {
		t.Errorf("Tables = %v, want both tables", r.Tables)
	}
	if !contains(r.WhereColumns, "counter_id") {
		t.Errorf("expected counter_id after GLOBAL IN, got %v", r.WhereColumns)
	}
}

func TestClickHouse_FinalNotAlias(t *testing.T) {
	r := chAnalyze(t, "SELECT * FROM db.events FINAL")
	for alias := range r.TableAliases {
		if alias == "final" {
			t.Errorf("FINAL misread as table alias: %v", r.TableAliases)
		}
	}
}

// ── Statement classification ──

func TestClickHouse_Classification(t *testing.T) {
	cases := []struct {
		sql      string
		class    StatementClass
		bypass   bool
		mutating bool
	}{
		{sql: "OPTIMIZE TABLE analytics.events FINAL", class: StatementOptimize, mutating: true},
		// SYSTEM/KILL are administration statements, not bypasses: they flow
		// through Tier 1 and are denied via rules.statement_blocklist.
		{sql: "SYSTEM FLUSH LOGS", class: StatementSystem},
		{sql: "SYSTEM RELOAD CONFIG", class: StatementSystem},
		{sql: "KILL QUERY WHERE query_id = 'abc'", class: StatementKill},
		{sql: "ATTACH TABLE db.events", class: StatementAttach, mutating: true},
		{sql: "DETACH TABLE db.events", class: StatementDetach, mutating: true},
		{sql: "RENAME TABLE analytics.events TO analytics.events_old", class: StatementRename, mutating: true},
		{sql: "EXCHANGE TABLES analytics.events AND analytics.events_old", class: StatementExchange, mutating: true},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			r := chAnalyze(t, tc.sql)
			if r.StatementClass != tc.class {
				t.Fatalf("StatementClass = %s, want %s", r.StatementClass, tc.class)
			}
			if IsBypass(r.StatementClass) != tc.bypass {
				t.Errorf("IsBypass(%s) = %v, want %v", r.StatementClass, IsBypass(r.StatementClass), tc.bypass)
			}
			if IsMutating(r.StatementClass) != tc.mutating {
				t.Errorf("IsMutating(%s) = %v, want %v", r.StatementClass, IsMutating(r.StatementClass), tc.mutating)
			}
		})
	}
}

func TestClickHouse_ClassNames(t *testing.T) {
	cases := map[StatementClass]string{
		StatementOptimize: "OPTIMIZE", StatementSystem: "SYSTEM",
		StatementAttach: "ATTACH", StatementDetach: "DETACH",
		StatementRename: "RENAME", StatementExchange: "EXCHANGE",
		StatementKill: "KILL",
	}
	for class, want := range cases {
		if got := class.String(); got != want {
			t.Errorf("StatementClass.String() = %q, want %q", got, want)
		}
	}
}

// ── Fail-open ──

func TestClickHouse_FailOpen(t *testing.T) {
	// Constructs the extractor cannot fully understand must yield partial
	// results and never an error that would reject the query.
	for _, sql := range []string{
		"SELECT @@@ FROM ???",
		"SELECT * FROM db.t WITH (bogus syntax (((",
		"SYSTEM ",
	} {
		res, err := For("clickhouse").Analyze(sql)
		if err != nil {
			t.Errorf("Analyze(%q) returned error %v, want nil (fail-open)", sql, err)
		}
		if res == nil {
			t.Errorf("Analyze(%q) returned nil result", sql)
		}
	}
}
