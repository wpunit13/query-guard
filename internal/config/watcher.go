package config

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ──────────────────────────────────────────────────────────────────────────────
// Watcher — Dynamic Policy Reloader
// ──────────────────────────────────────────────────────────────────────────────

// Watcher monitors a policy YAML file for changes and hot-reloads the shared
// Config without restarting the process or dropping active connections.
type Watcher struct {
	cfg      *Config
	path     string
	done     chan struct{}
	onReload func(*Policy) // optional callback invoked after every successful reload
}

// NewWatcher creates a Watcher that monitors the given file path.
// The provided Config is the thread-safe target that will be atomically updated.
func NewWatcher(cfg *Config, path string) *Watcher {
	return &Watcher{
		cfg:  cfg,
		path: path,
		done: make(chan struct{}),
	}
}

// OnReload sets a callback that is invoked with the freshly loaded Policy
// after every successful hot-reload.
func (w *Watcher) OnReload(fn func(*Policy)) {
	w.onReload = fn
}

// Start begins watching the policy file and blocks until the context is
// cancelled or Shutdown is called. Designed to run in its own goroutine.
func (w *Watcher) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("config/watcher: fsnotify init: %w", err)
	}
	defer watcher.Close()

	// Verify the file exists before watching.
	if _, err := os.Stat(w.path); os.IsNotExist(err) {
		return fmt.Errorf("config/watcher: policy file %q does not exist", w.path)
	}

	// fsnotify watches directories; add the parent dir of the target file.
	dir := parentDir(w.path)
	if err := watcher.Add(dir); err != nil {
		return fmt.Errorf("config/watcher: cannot add watch on %q: %w", dir, err)
	}

	log.Printf("[config/watcher] watching %q for changes", w.path)

	var debounceTimer *time.Timer

	for {
		select {
		case <-ctx.Done():
			log.Printf("[config/watcher] context cancelled, stopping watcher")
			return nil

		case <-w.done:
			log.Printf("[config/watcher] shutdown signal received, stopping watcher")
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// React only to WRITE events for our specific file; ignore CHMOD,
			// RENAME, etc.
			if event.Has(fsnotify.Write) && event.Name == w.path {
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(200*time.Millisecond, func() {
					if err := w.reload(); err != nil {
						log.Printf("[config/watcher] reload error: %v (keeping previous config)", err)
					}
				})
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Printf("[config/watcher] watch error: %v", err)
		}
	}
}

// Shutdown signals the watcher goroutine to exit cleanly.
func (w *Watcher) Shutdown() {
	close(w.done)
}

// reload re-reads the policy file from disk and atomically swaps it into the
// shared Config. On parse/validation failure the old config is preserved.
func (w *Watcher) reload() error {
	p, err := Load(w.path)
	if err != nil {
		return err
	}
	w.cfg.Set(p)
	log.Printf("[config/watcher] policy reloaded successfully from %q", w.path)
	if w.onReload != nil {
		w.onReload(p)
	}
	return nil
}

// parentDir returns the directory portion of a file path. Returns "." for
// bare filenames.
func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}
