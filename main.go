// Command airc is a conversation bus between Claude Code and Codex CLI sessions, driven by a human.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"

	"airc/pkg/bus"
	"airc/pkg/driver"
	"airc/pkg/provider"
	"airc/pkg/store"
	"airc/pkg/web"
)

var (
	Version        = "v0.0.0"
	CommitHash     = "0000000"
	BuildTimestamp = "1970-01-01T00:00:00"
	Builder        = "unknown"
	ProjectURL     = "unknown"
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return fallback
}

func newLogger() (*zap.Logger, error) {
	level, err := zapcore.ParseLevel(getEnv("LOG_LEVEL", "info"))
	if err != nil {
		level = zapcore.InfoLevel
	}
	var cfg zap.Config
	format := getEnv("LOG_FORMAT", "auto")
	if format == "console" || (format == "auto" && term.IsTerminal(int(os.Stdout.Fd()))) { //nolint:gosec // fd fits an int on every supported platform
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		cfg = zap.NewProductionConfig()
	}
	cfg.Level = zap.NewAtomicLevelAt(level)
	cfg.EncoderConfig.TimeKey = "timestamp"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return cfg.Build()
}

// adminToken comes from the environment, else from the database, else is generated once and kept.
func adminToken(ctx context.Context, st *store.Store) (string, bool, error) {
	if t := os.Getenv("AIRC_TOKEN"); t != "" {
		return t, false, nil
	}
	t, err := st.Setting(ctx, "admin_token")
	if err != nil {
		return "", false, err
	}
	if t != "" {
		return t, false, nil
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", false, err
	}
	t = hex.EncodeToString(b)
	return t, true, st.SetSetting(ctx, "admin_token", t)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "airc: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger, err := newLogger()
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer logger.Sync() //nolint:errcheck // stdout sync is best-effort
	logger.Info("startup", zap.String("version", Version), zap.String("commit_hash", CommitHash),
		zap.String("build_timestamp", BuildTimestamp), zap.String("builder", Builder), zap.String("project_url", ProjectURL))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(getEnv("AIRC_DB", "airc.db"))
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Error("store_close_failed", zap.Error(err))
		}
	}()
	token, generated, err := adminToken(ctx, st)
	if err != nil {
		return fmt.Errorf("admin token: %w", err)
	}
	if generated {
		logger.Warn("admin_token_generated", zap.String("token", token), zap.String("hint", "set AIRC_TOKEN to pin it"))
	}

	// Loopback by default: agents hold tokens and the admin token runs shell-spawning commands.
	addr := getEnv("AIRC_ADDR", "127.0.0.1:7667")
	b := bus.New(st)
	prometheus.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "airc_bus_dropped_total", Help: "Broadcasts lost to full subscriber buffers."},
		func() float64 { return float64(b.Dropped()) }))
	providers := []provider.Provider{
		provider.Claude{Bin: getEnv("AIRC_CLAUDE_BIN", "claude")},
		provider.Codex{Bin: getEnv("AIRC_CODEX_BIN", "codex")},
	}
	d := driver.New(b, providers, driver.Config{
		BaseURL:         getEnv("AIRC_URL", "http://"+addr),
		BotCap:          envInt("AIRC_BOT_CAP", 4),
		MaxTurnsAgent:   envInt("AIRC_MAX_TURNS_AGENT", 50),
		MaxTurnsChannel: envInt("AIRC_MAX_TURNS_CHANNEL", 200),
		TurnTimeout:     envDuration("AIRC_TURN_TIMEOUT", 15*time.Minute),
		ContextLimit:    envInt("AIRC_CONTEXT_LIMIT", 200),
		NtfyURL:         os.Getenv("AIRC_NTFY_URL"),
	}, logger)

	appSrv := &http.Server{
		Addr:    addr,
		Handler: web.New(b, token, logger).Handler(),
		// No WriteTimeout: /events is a long-lived SSE stream.
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:              getEnv("AIRC_METRICS_ADDR", "127.0.0.1:7668"),
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return web.Serve(gctx, appSrv, logger) })
	g.Go(func() error { return web.Serve(gctx, metricsSrv, logger) })
	g.Go(func() error {
		err := d.Run(gctx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	logger.Info("ready", zap.String("ui", "http://"+addr+"/"))
	err = g.Wait()
	logger.Info("shutdown")
	return err
}
