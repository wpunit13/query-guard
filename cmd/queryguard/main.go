package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"query-guard/internal/config"
	"query-guard/internal/engine"
	"query-guard/internal/proxy"
	"query-guard/internal/telemetry"
)

func main() {
	var policyPath string
	var port int
	flag.StringVar(&policyPath, "config", "policy.yaml", "path to the policy.yaml configuration file")
	flag.IntVar(&port, "port", 0, "override server.port from the policy (0 = use policy value)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	p, err := config.Load(policyPath)
	if err != nil {
		logger.Error("failed to load config", slog.String("path", policyPath), slog.String("error", err.Error()))
		os.Exit(1)
	}
	if port != 0 {
		p.Server.Port = port
	}

	cfg := config.NewConfig(p)

	// Telemetry.
	metrics := telemetry.NewMetrics(nil)

	// Pre-flight cost evaluator (Trino, fail-open on errors). It reads the
	// upstream and timeout from cfg on every evaluation, so those hot-reload.
	evaluator := engine.NewTrinoEvaluator(cfg, nil)
	evaluator.SetMetrics(metrics)

	// Core proxy handler.
	handler, err := proxy.NewHandler(cfg, evaluator, logger)
	if err != nil {
		logger.Error("failed to build handler", slog.String("error", err.Error()))
		os.Exit(1)
	}
	handler.SetMetrics(metrics)

	// Hot-reload watcher.
	watchCtx, cancelWatcher := context.WithCancel(context.Background())
	watcher := config.NewWatcher(cfg, policyPath)
	currentUpstream := cfg.Get().Upstream.URL
	watcher.OnReload(func(np *config.Policy) {
		logger.Info("policy reloaded",
			slog.Int("blocklist_entries", len(np.Rules.TableBlocklist)),
			slog.Int("cost_limits", len(np.Rules.CostLimits)))
		// Rebuild the reverse proxy if the upstream changed.
		if np.Upstream.URL != currentUpstream {
			if err := handler.UpdateUpstream(np.Upstream.URL); err != nil {
				logger.Error("failed to update upstream", slog.String("upstream", np.Upstream.URL), slog.String("error", err.Error()))
			} else {
				logger.Info("upstream updated (hot-reload)", slog.String("upstream", np.Upstream.URL))
				currentUpstream = np.Upstream.URL
			}
		}
	})
	go func() {
		if err := watcher.Start(watchCtx); err != nil {
			logger.Warn("config watcher stopped", slog.String("error", err.Error()))
		}
	}()

	addr := fmt.Sprintf(":%d", p.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      hstsHandler(handler, p.Server.TLS.Enabled()),
		ReadTimeout:  p.Server.ReadTimeout,
		WriteTimeout: p.Server.WriteTimeout,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if p.Server.TLS.Enabled() {
			// HTTPS-only: no plaintext listener exists, so Authorization and
			// X-Trino-* headers are never transmitted unencrypted.
			logger.Info("query-guard listening", "scheme", "https", "addr", addr, "upstream", p.Upstream.URL)
			if err := srv.ListenAndServeTLS(p.Server.TLS.CertFile, p.Server.TLS.KeyFile); err != nil && err != http.ErrServerClosed {
				logger.Error("https server error", slog.String("error", err.Error()))
				os.Exit(1)
			}
		} else {
			logger.Info("query-guard listening", "scheme", "http", "addr", addr, "upstream", p.Upstream.URL)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("http server error", slog.String("error", err.Error()))
				os.Exit(1)
			}
		}
	}()

	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig.String())

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), p.Server.ShutdownGrace)
	defer cancelShutdown()
	cancelWatcher()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown error", slog.String("error", err.Error()))
	}
	logger.Info("shutdown complete")
}

// hstsHandler wraps the root handler with a Strict-Transport-Security header
// when TLS is enabled, so clients pin to HTTPS after the first response.
// It is a no-op wrapper when TLS is disabled (HSTS over plaintext would break
// HTTP clients without adding security).
func hstsHandler(next http.Handler, tlsEnabled bool) http.Handler {
	if !tlsEnabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}
