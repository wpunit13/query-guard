package main

import (
	"context"
	"flag"
	"fmt"
	"log"
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

	logger := log.New(os.Stdout, "[query-guard] ", log.LstdFlags|log.Lmsgprefix)

	p, err := config.Load(policyPath)
	if err != nil {
		logger.Fatalf("failed to load config %q: %v", policyPath, err)
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

	// Core proxy handler.
	handler, err := proxy.NewHandler(cfg, evaluator, logger)
	if err != nil {
		logger.Fatalf("failed to build handler: %v", err)
	}
	handler.SetMetrics(metrics)

	// Hot-reload watcher.
	watchCtx, cancelWatcher := context.WithCancel(context.Background())
	watcher := config.NewWatcher(cfg, policyPath)
	currentUpstream := cfg.Get().Upstream.URL
	watcher.OnReload(func(np *config.Policy) {
		logger.Printf("policy reloaded: %d blocklist entries, %d cost limits",
			len(np.Rules.TableBlocklist), len(np.Rules.CostLimits))
		// Rebuild the reverse proxy if the upstream changed.
		if np.Upstream.URL != currentUpstream {
			if err := handler.UpdateUpstream(np.Upstream.URL); err != nil {
				logger.Printf("failed to update upstream to %q: %v", np.Upstream.URL, err)
			} else {
				logger.Printf("upstream updated to %q (hot-reload)", np.Upstream.URL)
				currentUpstream = np.Upstream.URL
			}
		}
	})
	go func() {
		if err := watcher.Start(watchCtx); err != nil {
			logger.Printf("config watcher stopped: %v", err)
		}
	}()

	addr := fmt.Sprintf(":%d", p.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  p.Server.ReadTimeout,
		WriteTimeout: p.Server.WriteTimeout,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Printf("query-guard listening on %s, proxying to %s", addr, p.Upstream.URL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("http server error: %v", err)
		}
	}()

	sig := <-sigCh
	logger.Printf("received signal %s, shutting down", sig)

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), p.Server.ShutdownGrace)
	defer cancelShutdown()
	cancelWatcher()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("shutdown error: %v", err)
	}
	logger.Printf("shutdown complete")
}
