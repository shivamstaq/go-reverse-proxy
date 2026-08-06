package config

import (
	"log/slog"
	"strings"
	"testing"
)

// setMinimal supplies only what Load refuses to default.
func setMinimal(t *testing.T) {
	t.Helper()
	t.Setenv("KEYWORD", "test-keyword")
	t.Setenv("REPLACEMENT", "REDACTED")
}

func TestLoadDefaults(t *testing.T) {
	setMinimal(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ListenAddr != ":8443" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if got := cfg.Upstream.String(); got != "https://localhost:9443" {
		t.Errorf("Upstream = %q", got)
	}
	if cfg.CACertFile != "certs/ca.crt" || cfg.TLSCertFile != "certs/proxy.crt" || cfg.TLSKeyFile != "certs/proxy.key" {
		t.Errorf("cert paths = %q %q %q", cfg.CACertFile, cfg.TLSCertFile, cfg.TLSKeyFile)
	}
}

func TestLoadOverrides(t *testing.T) {
	setMinimal(t)
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("UPSTREAM_URL", "https://origin.internal:443/base")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.Upstream.Host != "origin.internal:443" {
		t.Errorf("Upstream.Host = %q", cfg.Upstream.Host)
	}
}

func TestLoadLogLevel(t *testing.T) {
	tests := []struct {
		value string
		want  slog.Level
	}{
		{"", slog.LevelInfo}, // the zero value, which is Info
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
	}

	for _, tc := range tests {
		t.Run("level "+tc.value, func(t *testing.T) {
			setMinimal(t)
			t.Setenv("LOG_LEVEL", tc.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LogLevel != tc.want {
				t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, tc.want)
			}
		})
	}
}

// An empty value must fall back rather than yield "".
func TestLoadEmptyValueFallsBack(t *testing.T) {
	setMinimal(t)
	t.Setenv("LISTEN_ADDR", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":8443" {
		t.Errorf("ListenAddr = %q, want the default", cfg.ListenAddr)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing keyword", map[string]string{"KEYWORD": ""}, "KEYWORD is required"},
		{"missing replacement", map[string]string{"REPLACEMENT": ""}, "REPLACEMENT is required"},
		{"plaintext upstream", map[string]string{"UPSTREAM_URL": "http://origin:8080"}, "scheme must be https"},
		{"schemeless upstream", map[string]string{"UPSTREAM_URL": "origin:8080"}, "scheme must be https"},
		{"hostless upstream", map[string]string{"UPSTREAM_URL": "https:///path"}, "missing host"},
		{"unparseable upstream", map[string]string{"UPSTREAM_URL": "https://%zz"}, "UPSTREAM_URL"},
		{"unknown log level", map[string]string{"LOG_LEVEL": "chatty"}, "LOG_LEVEL"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setMinimal(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			_, err := Load()
			if err == nil {
				t.Fatalf("Load succeeded, want error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// Every problem must surface from a single call.
func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	t.Setenv("KEYWORD", "")
	t.Setenv("REPLACEMENT", "")
	t.Setenv("UPSTREAM_URL", "http://origin:8080")

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded, want errors")
	}
	for _, want := range []string{"KEYWORD", "REPLACEMENT", "https"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %q", err, want)
		}
	}
}
