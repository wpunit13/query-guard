package config

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// Watcher Tests
// ──────────────────────────────────────────────────────────────────────────────

func TestWatcher_ReloadOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")

	writeFile(t, path, validPolicyYAML())
	p, err := Load(path)
	if err != nil {
		t.Fatalf("initial Load() error: %v", err)
	}

	cfg := NewConfig(p)
	watcher := NewWatcher(cfg, path)

	reloadCh := make(chan *Policy, 1)
	watcher.OnReload(func(p *Policy) {
		reloadCh <- p
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	go func() {
		if err := watcher.Start(ctx); err != nil {
			t.Logf("watcher.Start returned: %v", err)
		}
	}()

	// Give fsnotify time to register the watch
	time.Sleep(200 * time.Millisecond)

	// Write modified policy (port changes to 9090)
	writeFile(t, path, modifiedPolicyYAML())

	select {
	case p2 := <-reloadCh:
		if p2.Server.Port != 9090 {
			t.Errorf("expected port 9090 after reload, got %d", p2.Server.Port)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for file-watch reload")
	}

	watcher.Shutdown()
}

func TestWatcher_InvalidYAMLKeepsOldConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")

	writeFile(t, path, validPolicyYAML())
	p, err := Load(path)
	if err != nil {
		t.Fatalf("initial Load() error: %v", err)
	}

	cfg := NewConfig(p)
	watcher := NewWatcher(cfg, path)

	reloadCh := make(chan *Policy, 1)
	watcher.OnReload(func(p *Policy) {
		reloadCh <- p
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	time.Sleep(200 * time.Millisecond)

	// Overwrite with invalid YAML — OnReload must NOT fire
	writeFile(t, path, "[[[ invalid yaml }}}")

	// Wait to confirm no reload happens
	time.Sleep(1 * time.Second)

	// Config must still hold original port 8080
	after := cfg.Get()
	if after.Server.Port != 8080 {
		t.Errorf("expected original port 8080 preserved, got %d", after.Server.Port)
	}

	watcher.Shutdown()
}

func TestWatcher_Shutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	writeFile(t, path, validPolicyYAML())

	p, _ := Load(path)
	cfg := NewConfig(p)
	watcher := NewWatcher(cfg, path)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	doneCh := make(chan struct{})
	go func() {
		if err := watcher.Start(ctx); err != nil {
			t.Logf("watcher.Start returned: %v", err)
		}
		close(doneCh)
	}()

	time.Sleep(100 * time.Millisecond)

	// Shutdown should cause Start to return
	watcher.Shutdown()

	select {
	case <-doneCh:
		// success — Start exited after Shutdown
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watcher to shut down")
	}
}

func TestWatcher_OnReloadNil(t *testing.T) {
	// Verify OnReload never panics when callback is nil
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	writeFile(t, path, validPolicyYAML())

	p, _ := Load(path)
	cfg := NewConfig(p)
	watcher := NewWatcher(cfg, path)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	time.Sleep(200 * time.Millisecond)
	writeFile(t, path, modifiedPolicyYAML())
	time.Sleep(500 * time.Millisecond)

	watcher.Shutdown()
}

// ──────────────────────────────────────────────────────────────────────────────
// Helper — Modified YAML
// ──────────────────────────────────────────────────────────────────────────────

func modifiedPolicyYAML() string {
	return `
server:
  port: 9090

upstream:
  url: "http://trino-coordinator:8080"

preflight:
  timeout: 2s
  max_concurrent: 5

rules:
  cost_limits:
    - catalog: "hive"
      schema: "default"
      table: "orders"
      max_scan_bytes: 1073741824
      max_rows: 10000000

telemetry:
  enabled: true
  path: "/metrics"
`
}
