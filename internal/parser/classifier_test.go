package parser

import (
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────────
// Classifier tests
// ──────────────────────────────────────────────────────────────────────────────

func TestClassify_Select(t *testing.T) {
	if c := Classify("SELECT * FROM orders"); c != StatementSelect {
		t.Errorf("expected SELECT, got %s", c)
	}
}

func TestClassify_SelectWithLeadingComment(t *testing.T) {
	if c := Classify("-- comment\nSELECT * FROM orders"); c != StatementSelect {
		t.Errorf("expected SELECT, got %s", c)
	}
}

func TestClassify_SelectWithBlockComment(t *testing.T) {
	if c := Classify("/* block */ SELECT * FROM orders"); c != StatementSelect {
		t.Errorf("expected SELECT, got %s", c)
	}
}

func TestClassify_WithCTE(t *testing.T) {
	if c := Classify("WITH cte AS (SELECT 1) SELECT * FROM cte"); c != StatementSelect {
		t.Errorf("expected SELECT (WITH), got %s", c)
	}
}

func TestClassify_WithDelete(t *testing.T) {
	if c := Classify("WITH cte AS (SELECT 1) DELETE FROM t"); c != StatementDelete {
		t.Errorf("expected DELETE (outermost verb), got %s", c)
	}
}

func TestClassify_WithInsert(t *testing.T) {
	if c := Classify("WITH cte AS (SELECT 1) INSERT INTO t SELECT * FROM cte"); c != StatementInsert {
		t.Errorf("expected INSERT (outermost verb), got %s", c)
	}
}

func TestClassify_WithUpdate(t *testing.T) {
	if c := Classify("WITH cte AS (SELECT 1) UPDATE t SET a = 1"); c != StatementUpdate {
		t.Errorf("expected UPDATE (outermost verb), got %s", c)
	}
}

func TestClassify_Insert(t *testing.T) {
	if c := Classify("INSERT INTO t VALUES (1)"); c != StatementInsert {
		t.Errorf("expected INSERT, got %s", c)
	}
}

func TestClassify_Update(t *testing.T) {
	if c := Classify("UPDATE t SET a=1"); c != StatementUpdate {
		t.Errorf("expected UPDATE, got %s", c)
	}
}

func TestClassify_Delete(t *testing.T) {
	if c := Classify("DELETE FROM t"); c != StatementDelete {
		t.Errorf("expected DELETE, got %s", c)
	}
}

func TestClassify_Show(t *testing.T) {
	if c := Classify("SHOW TABLES"); c != StatementShow {
		t.Errorf("expected SHOW, got %s", c)
	}
}

func TestClassify_Describe(t *testing.T) {
	if c := Classify("DESCRIBE t"); c != StatementDescribe {
		t.Errorf("expected DESCRIBE, got %s", c)
	}
	if c := Classify("DESC t"); c != StatementDescribe {
		t.Errorf("expected DESCRIBE (DESC), got %s", c)
	}
}

func TestClassify_Explain(t *testing.T) {
	if c := Classify("EXPLAIN SELECT 1"); c != StatementExplain {
		t.Errorf("expected EXPLAIN, got %s", c)
	}
}

func TestClassify_Use(t *testing.T) {
	if c := Classify("USE default"); c != StatementUse {
		t.Errorf("expected USE, got %s", c)
	}
}

func TestClassify_Set(t *testing.T) {
	if c := Classify("SET SESSION x = 1"); c != StatementSet {
		t.Errorf("expected SET, got %s", c)
	}
}

func TestClassify_Create(t *testing.T) {
	if c := Classify("CREATE TABLE t (id INT)"); c != StatementCreate {
		t.Errorf("expected CREATE, got %s", c)
	}
}

func TestClassify_Alter(t *testing.T) {
	if c := Classify("ALTER TABLE t ADD COLUMN x INT"); c != StatementAlter {
		t.Errorf("expected ALTER, got %s", c)
	}
}

func TestClassify_Drop(t *testing.T) {
	if c := Classify("DROP TABLE t"); c != StatementDrop {
		t.Errorf("expected DROP, got %s", c)
	}
}

func TestClassify_Truncate(t *testing.T) {
	if c := Classify("TRUNCATE TABLE t"); c != StatementTruncate {
		t.Errorf("expected TRUNCATE, got %s", c)
	}
}

func TestClassify_Call(t *testing.T) {
	if c := Classify("CALL foo()"); c != StatementCall {
		t.Errorf("expected CALL, got %s", c)
	}
}

func TestClassify_Empty(t *testing.T) {
	if c := Classify(""); c != StatementUnknown {
		t.Errorf("expected UNKNOWN, got %s", c)
	}
}

func TestClassify_Other(t *testing.T) {
	if c := Classify("GRANT SELECT ON t TO user"); c != StatementOther {
		t.Errorf("expected OTHER, got %s", c)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// IsBypass tests
// ──────────────────────────────────────────────────────────────────────────────

func TestIsBypass(t *testing.T) {
	tests := []struct {
		class  StatementClass
		bypass bool
	}{
		{StatementSelect, false},
		{StatementInsert, false},
		{StatementUpdate, false},
		{StatementDelete, false},
		{StatementCreate, false},
		{StatementAlter, false},
		{StatementDrop, false},
		{StatementShow, true},
		{StatementDescribe, true},
		{StatementExplain, true},
		{StatementUse, true},
		{StatementSet, true},
		{StatementTruncate, false},
		{StatementOther, false},
		{StatementUnknown, false},
	}
	for _, tc := range tests {
		got := IsBypass(tc.class)
		if got != tc.bypass {
			t.Errorf("IsBypass(%s) = %v, want %v", tc.class, got, tc.bypass)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// IsMutating tests
// ──────────────────────────────────────────────────────────────────────────────

func TestIsMutating(t *testing.T) {
	tests := []struct {
		class    StatementClass
		mutating bool
	}{
		{StatementSelect, false},
		{StatementInsert, true},
		{StatementUpdate, true},
		{StatementDelete, true},
		{StatementCreate, true},
		{StatementAlter, true},
		{StatementDrop, true},
		{StatementTruncate, true},
		{StatementShow, false},
	}
	for _, tc := range tests {
		got := IsMutating(tc.class)
		if got != tc.mutating {
			t.Errorf("IsMutating(%s) = %v, want %v", tc.class, got, tc.mutating)
		}
	}
}
