package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
)

type Config struct {
	ListenAddr  string     // client-facing address, e.g. ":8443"
	Upstream    *url.URL   // upstream base URL; ensure HTTPS for TLS termination
	CACertFile  string     // CA that signed the upstream certificate
	TLSCertFile string     // certificate presented to clients
	TLSKeyFile  string     // private key for TLSCertFile
	Keyword     string     // token to replace in textual responses
	Replacement string     // what to replace it with
	LogLevel    slog.Level // debug exposes the rewriter's per-response decisions
}

// Load reads the environment and validates it. [fail-fast]
func Load() (Config, error) {
	cfg := Config{
		ListenAddr:  getenv("LISTEN_ADDR", ":8443"),
		CACertFile:  getenv("CA_CERT_FILE", "certs/ca.crt"),
		TLSCertFile: getenv("TLS_CERT_FILE", "certs/proxy.crt"),
		TLSKeyFile:  getenv("TLS_KEY_FILE", "certs/proxy.key"),
		Keyword:     os.Getenv("KEYWORD"),
		Replacement: os.Getenv("REPLACEMENT"),
	}

	var problems []error

	raw := getenv("UPSTREAM_URL", "https://localhost:9443")
	upstream, err := url.Parse(raw)

	switch {
	case err != nil:
		problems = append(problems, fmt.Errorf("UPSTREAM_URL %q: %w", raw, err))
	case upstream.Scheme != "https":
		problems = append(problems, fmt.Errorf("UPSTREAM_URL %q: scheme must be https", raw))
	case upstream.Host == "":
		problems = append(problems, fmt.Errorf("UPSTREAM_URL %q: missing host", raw))
	default:
		cfg.Upstream = upstream
	}

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			problems = append(problems, fmt.Errorf("LOG_LEVEL %q: want debug, info, warn or error", v))
		}
	}

	// * Keyword and replacement are REQUIRED
	if cfg.Keyword == "" {
		problems = append(problems, errors.New("KEYWORD is required"))
	}
	if cfg.Replacement == "" {
		problems = append(problems, errors.New("REPLACEMENT is required"))
	}

	return cfg, errors.Join(problems...)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
