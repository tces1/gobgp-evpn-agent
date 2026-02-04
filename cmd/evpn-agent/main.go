package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"gobgp-evpn-agent/internal/agent"
	"gobgp-evpn-agent/internal/config"
)

func main() {
	cfgPath := flag.String("config", "/etc/evpn-agent/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	setupLogger(cfg.LogLevel)

	ag, err := agent.New(cfg)
	if err != nil {
		slog.Error("failed to init agent", "err", err)
		os.Exit(1)
	}
	defer ag.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := ag.Run(ctx); err != nil {
		slog.Error("agent exited with error", "err", err)
		os.Exit(1)
	}
}

func setupLogger(level string) {
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(h))
}
