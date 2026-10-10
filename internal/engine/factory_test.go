package engine

import (
	"net/http"
	"testing"

	"query-guard/internal/config"
)

// ──────────────────────────────────────────────────────────────────────────────
// Engine registry tests
// ──────────────────────────────────────────────────────────────────────────────

func TestNew_TrinoRegistered(t *testing.T) {
	e, err := New(config.NewConfig(testPolicy()), "trino", nil)
	if err != nil {
		t.Fatalf("New(trino) unexpected error: %v", err)
	}
	if e.Engine() != "trino" {
		t.Errorf("Engine() = %q, want trino", e.Engine())
	}
	if got := e.StatementPath(); got != "/v1/statement" {
		t.Errorf("StatementPath() = %q, want /v1/statement", got)
	}
}

func TestNew_UnknownEngineFailsLoudly(t *testing.T) {
	// An engine that is configured but not linked into the binary must fail
	// at construction, not silently degrade.
	if _, err := New(config.NewConfig(testPolicy()), "spark", nil); err == nil {
		t.Fatal("expected error for unregistered engine, got nil")
	}
}

func TestRegister_DuplicateNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	Register("dup-test-engine", func(*config.Config, *http.Client) (Engine, error) {
		return nil, nil
	})
	Register("dup-test-engine", func(*config.Config, *http.Client) (Engine, error) {
		return nil, nil
	})
}

func TestRegister_NilConstructorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil constructor")
		}
	}()
	Register("nil-ctor-test-engine", nil)
}

func TestSetMetrics_AttachesToCapableEngines(t *testing.T) {
	e, err := New(config.NewConfig(testPolicy()), "trino", nil)
	if err != nil {
		t.Fatalf("New(trino) unexpected error: %v", err)
	}
	// Must not panic: TrinoEvaluator implements MetricsSetter.
	SetMetrics(e, nil)
}
