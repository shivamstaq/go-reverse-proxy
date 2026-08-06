package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"go-reverse-proxy/internal/config"
	"go-reverse-proxy/internal/rewriter"
)

const (
	// Client-facing.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second

	// Upstream leg.
	dialTimeout           = 5 * time.Second
	tlsHandshakeTimeout   = 5 * time.Second
	responseHeaderTimeout = 30 * time.Second
	idleConnTimeout       = 90 * time.Second
	keepAlive             = 30 * time.Second
	maxIdleConns          = 100
)

// NewHandler builds the proxy's request pipeling
//   - logging
//   - panic recovery
//   - host rewriting
//   - forwarding to the upstream
//   - rewriting the response on the way back
func NewHandler(cfg config.Config) (http.Handler, error) {
	transport, err := upstreamTransport(cfg.CACertFile)
	if err != nil {
		return nil, err
	}

	rw, err := rewriter.New(cfg.Keyword, cfg.Replacement)
	if err != nil {
		return nil, err
	}

	e := echo.New()
	e.Logger = slog.Default()

	// Pre to catch request before it reaches router
	e.Pre(
		middleware.RequestLogger(),
		// Create a request ID for tracking each request
		middleware.RequestID(),
		middleware.Recover(),
		rejectConnect(),
		forwardHost(cfg.Upstream),
		middleware.ProxyWithConfig(middleware.ProxyConfig{
			Balancer: middleware.NewRoundRobinBalancer([]*middleware.ProxyTarget{{
				Name: "upstream",
				URL:  cfg.Upstream,
			}}),
			Transport:      transport,
			ModifyResponse: rw.Apply,
		}),
	)

	return e, nil
}

// explicitly reject CONNECT requests- unsupported
func rejectConnect() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if c.Request().Method == http.MethodConnect {
				return echo.NewHTTPError(http.StatusNotImplemented, "CONNECT tunnelling is not supported")
			}
			return next(c)
		}
	}
}

// Start terminates client TLS and serves handler until ctx is cancelled.
func Start(ctx context.Context, cfg config.Config, handler http.Handler) error {
	cert, err := os.ReadFile(cfg.TLSCertFile)
	if err != nil {
		return fmt.Errorf("read TLS cert %q: %w", cfg.TLSCertFile, err)
	}
	key, err := os.ReadFile(cfg.TLSKeyFile)
	if err != nil {
		return fmt.Errorf("read TLS key %q: %w", cfg.TLSKeyFile, err)
	}

	start := echo.StartConfig{
		Address: cfg.ListenAddr,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Echo defaults to h2 only, which would reject HTTP/1.1 clients.
			NextProtos: []string{"h2", "http/1.1"},
		},
	}

	if err := start.StartTLS(ctx, handler, cert, key); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// forwardHost points the outgoing Host header at the upstream so virtual-hosted
// origins route to the right site, keeping the client's value in X-Forwarded-Host.
func forwardHost(upstream *url.URL) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			req := c.Request()

			// Only when absent - existing upstream chain survives
			if req.Header.Get("X-Forwarded-Host") == "" && req.Host != "" {
				req.Header.Set("X-Forwarded-Host", req.Host)
			}

			// Restored on the way out
			clientHost := req.Host
			defer func() { req.Host = clientHost }()

			req.Host = upstream.Host
			return next(c)
		}
	}
}

// upstreamTransport dials the upstream over TLS, trusting only the internal CA.
func upstreamTransport(caFile string) (*http.Transport, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", caFile, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates found in %q", caFile)
	}

	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
		ForceAttemptHTTP2: true,
		// Left enabled, Go adds an Accept-Encoding the client never asked for and
		// then transparently decompresses the reply, stripping Content-Encoding.
		// A proxy must forward the client's encoding preferences untouched — and
		// the rewriter can only re-emit a gzip body if it is handed one.
		DisableCompression: true,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: keepAlive,
		}).DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConns,
		IdleConnTimeout:       idleConnTimeout,
	}, nil
}
