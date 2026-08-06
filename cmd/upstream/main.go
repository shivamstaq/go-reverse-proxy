package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/labstack/echo/v5"

	"go-reverse-proxy/internal/fixtures"
)

func main() {
	cert, err := os.ReadFile(getenv("TLS_CERT_FILE", "certs/upstream.crt"))
	if err != nil {
		slog.Error("read TLS cert", "error", err)
		os.Exit(1)
	}

	key, err := os.ReadFile(getenv("TLS_KEY_FILE", "certs/upstream.key"))
	if err != nil {
		slog.Error("read TLS key", "error", err)
		os.Exit(1)
	}

	start := echo.StartConfig{
		Address: getenv("LISTEN_ADDR", ":9443"),
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1"},
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := start.StartTLS(ctx, fixtures.NewServer(), cert, key); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("upstream server stopped", "error", err)
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
