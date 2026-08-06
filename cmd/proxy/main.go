package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"go-reverse-proxy/internal/config"
	"go-reverse-proxy/internal/proxy"
)

func main() {
	level := new(slog.LevelVar)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	level.Set(cfg.LogLevel)

	handler, err := proxy.NewHandler(cfg)
	if err != nil {
		slog.Error("build proxy", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting proxy", "address", cfg.ListenAddr, "upstream", cfg.Upstream.String())

	if err := proxy.Start(ctx, cfg, handler); err != nil {
		slog.Error("proxy server stopped", "error", err)
		os.Exit(1)
	}
}
