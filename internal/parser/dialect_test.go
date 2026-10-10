package parser

import "testing"

// ──────────────────────────────────────────────────────────────────────────────
// Dialect registry tests
// ──────────────────────────────────────────────────────────────────────────────

func TestFor_Trino(t *testing.T) {
	d := For("trino")
	if d == nil {
		t.Fatal("For(trino) returned nil dialect")
	}
	if d.Name() != "trino" {
		t.Errorf("Name() = %q, want trino", d.Name())
	}
}

func TestFor_UnknownEngineFallsBackToTrino(t *testing.T) {
	// Engines whose dialect is not linked into the binary still get Tier 1
	// coverage via the trino fallback (fail-open), never a nil dialect.
	for _, name := range []string{"cockroachdb", "snowflake", "spark"} {
		d := For(name)
		if d == nil {
			t.Fatalf("For(%q) returned nil dialect", name)
		}
		if d.Name() != "trino" {
			t.Errorf("For(%q).Name() = %q, want trino fallback", name, d.Name())
		}
	}
}

func TestFor_ClickHouse(t *testing.T) {
	d := For("clickhouse")
	if d == nil {
		t.Fatal("For(clickhouse) returned nil dialect")
	}
	if d.Name() != "clickhouse" {
		t.Errorf("Name() = %q, want clickhouse", d.Name())
	}
}

func TestTrinoDialect_MatchesPackageAnalyze(t *testing.T) {
	sql := "SELECT * FROM tpch.tiny.nation WHERE nationkey = 1"

	viaDialect, err := For("trino").Analyze(sql)
	if err != nil {
		t.Fatalf("dialect Analyze error: %v", err)
	}
	direct, err := Analyze(sql)
	if err != nil {
		t.Fatalf("package Analyze error: %v", err)
	}

	if got, want := viaDialect.StatementClass, direct.StatementClass; got != want {
		t.Errorf("StatementClass = %v, want %v", got, want)
	}
	if got, want := len(viaDialect.Tables), len(direct.Tables); got != want {
		t.Fatalf("Tables count = %d, want %d", got, want)
	}
	for i := range direct.Tables {
		if viaDialect.Tables[i] != direct.Tables[i] {
			t.Errorf("Tables[%d] = %q, want %q", i, viaDialect.Tables[i], direct.Tables[i])
		}
	}
	if got, want := len(viaDialect.WhereColumns), len(direct.WhereColumns); got != want {
		t.Errorf("WhereColumns count = %d, want %d", got, want)
	}
}

func TestTrinoDialect_BypassStatement(t *testing.T) {
	res, err := For("trino").Analyze("SHOW TABLES FROM tpch.tiny")
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	if !IsBypass(res.StatementClass) {
		t.Errorf("StatementClass = %v, want a bypass class", res.StatementClass)
	}
}

func TestRegister_DuplicateNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	Register("dup-test-dialect", trinoDialect{})
	Register("dup-test-dialect", trinoDialect{})
}

func TestRegister_NilDialectPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil dialect")
		}
	}()
	Register("nil-dialect-test", nil)
}
