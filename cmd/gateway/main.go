// Command gateway is the multichain JSON-RPC gateway entrypoint.
//
// It loads configuration once at startup, assembles the router and HTTP
// handlers, exposes POST / for JSON-RPC on the server listener and
// /metrics + /healthz on the metrics listener, and shuts down gracefully on
// SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xtianxx/multichain-rpc-gateway/internal/api"
	"github.com/xtianxx/multichain-rpc-gateway/internal/config"
	"github.com/xtianxx/multichain-rpc-gateway/internal/logging"
	"github.com/xtianxx/multichain-rpc-gateway/internal/metrics"
	"github.com/xtianxx/multichain-rpc-gateway/internal/prober"
	"github.com/xtianxx/multichain-rpc-gateway/internal/router"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML config file")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	flag.Parse()

	var level slog.Level
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := logging.New(level)
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}

	// Explicit registry wired with the standard go/process collectors, per
	// the metrics contract; the gateway collectors register on top.
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	metrics.Register(reg)

	rt, err := router.New(cfg, logger)
	if err != nil {
		logger.Error("build router", "error", err)
		os.Exit(1)
	}

	handler := api.New(rt, cfg.Server.MaxBodyBytes, cfg.Server.MaxBatchElements, logger)

	rpcMux := http.NewServeMux()
	rpcMux.Handle("/", handler)
	rpcServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           rpcMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	metricsServer := &http.Server{
		Addr:              cfg.Server.MetricsListen,
		Handler:           metricsMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Active upstream health probing (US3): eth_chainId probes feed the
	// health state machine and the circuit breakers. Runs until shutdown.
	pr := prober.New(rt.Chains(), cfg.Prober, logger)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pr.Start(ctx)
	}()

	errCh := make(chan error, 2)
	go func() { errCh <- rpcServer.ListenAndServe() }()
	go func() { errCh <- metricsServer.ListenAndServe() }()
	logger.Info("gateway started",
		"listen", cfg.Server.Listen,
		"metrics_listen", cfg.Server.MetricsListen,
		"chains", len(cfg.Chains),
	)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rpcServer.Shutdown(shutdownCtx)
		_ = metricsServer.Shutdown(shutdownCtx)
		// pr.Start observes the signal ctx and returns when it is
		// cancelled; wait for the probe loop to unwind before exiting.
		wg.Wait()
		logger.Info("gateway stopped")
	}
}
