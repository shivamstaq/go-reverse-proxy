package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"go-reverse-proxy/internal/fixtures"
	"go-reverse-proxy/internal/testca"
)

// startedProxy is a proxy served over its own TLS listener, so the client-facing
// hop is real HTTPS rather than httptest's plaintext stand-in.
type startedProxy struct {
	addr   string
	caPool *x509.CertPool
	done   <-chan error // receives Start's return value after shutdown
	stop   context.CancelFunc
}

func startProxy(t *testing.T) startedProxy {
	t.Helper()

	ca := testca.New(t)
	dir := t.TempDir()
	loopback := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}

	upstream := httptest.NewUnstartedServer(fixtures.NewServer())
	upstream.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.Leaf(t, "upstream", []string{"localhost"}, loopback)},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	cfg := testConfig(t, upstream.URL, ca.WriteCertFile(t, dir))
	cfg.TLSCertFile, cfg.TLSKeyFile = ca.WriteLeafFiles(t, dir, "proxy", []string{"localhost"}, loopback)
	cfg.ListenAddr = freeAddr(t)

	handler, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Start(ctx, cfg, handler) }()

	t.Cleanup(stop)
	waitForListener(t, cfg.ListenAddr)

	return startedProxy{addr: cfg.ListenAddr, caPool: ca.Pool, done: done, stop: stop}
}

func (p startedProxy) url(path string) string { return "https://" + p.addr + path }

// freeAddr picks a port the kernel says is free. Start takes its address from
// configuration, so the port has to be known before the server begins listening.
func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer l.Close()

	return l.Addr().String()
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("proxy never started listening on %s", addr)
}

func tlsClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
			ForceAttemptHTTP2:  true,
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

// The whole path end to end: HTTPS in, TLS to the upstream, rewritten on the way
// back out.
func TestStartTerminatesClientTLS(t *testing.T) {
	p := startProxy(t)

	resp, body := get(t, tlsClient(p.caPool), p.url("/html"))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Fatal("connection was not TLS")
	}
	if resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("negotiated TLS version %#x, want at least TLS 1.2", resp.TLS.Version)
	}
	if bytes.Contains(body, []byte(fixtures.Keyword)) {
		t.Errorf("body was not rewritten over the TLS path: %s", body)
	}
}

// Listing http/1.1 alongside h2 is what keeps HTTP/1.1-only clients working;
// Echo's own default is h2 alone.
func TestStartServesBothHTTPVersions(t *testing.T) {
	p := startProxy(t)

	tests := []struct {
		name      string
		nextProto []string
		want      int
	}{
		{"http/2", []string{"h2"}, 2},
		{"http/1.1", []string{"http/1.1"}, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{
				Transport: &http.Transport{
					ForceAttemptHTTP2: tc.want == 2,
					TLSClientConfig: &tls.Config{
						RootCAs:    p.caPool,
						MinVersion: tls.VersionTLS12,
						NextProtos: tc.nextProto,
					},
				},
			}

			resp, _ := get(t, client, p.url("/json"))
			if resp.ProtoMajor != tc.want {
				t.Errorf("negotiated HTTP/%d, want HTTP/%d", resp.ProtoMajor, tc.want)
			}
		})
	}
}

// A client that does not trust the CA must be refused, or the certificate is
// decoration rather than proof.
func TestStartRejectsClientWithoutCA(t *testing.T) {
	p := startProxy(t)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    x509.NewCertPool(), // deliberately empty
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	resp, err := client.Get(p.url("/html"))
	if err == nil {
		resp.Body.Close()
		t.Fatal("request succeeded without the CA")
	}
}

// TLS 1.0/1.1 must be refused. The client trusts the CA, so the protocol version
// is the only thing that can fail the handshake.
func TestStartRejectsObsoleteTLSVersions(t *testing.T) {
	p := startProxy(t)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    p.caPool,
				MinVersion: tls.VersionTLS10,
				MaxVersion: tls.VersionTLS11,
			},
		},
	}

	resp, err := client.Get(p.url("/html"))
	if err == nil {
		resp.Body.Close()
		t.Fatal("server accepted a TLS 1.1 client")
	}
}

// Cancelling the context must shut the server down rather than leave it running.
func TestStartShutsDownOnContextCancel(t *testing.T) {
	p := startProxy(t)

	p.stop()

	select {
	case err := <-p.done:
		if err != nil {
			t.Errorf("Start returned %v, want nil on graceful shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Start did not return after the context was cancelled")
	}
}

func TestStartRejectsMissingKeypair(t *testing.T) {
	dir := t.TempDir()
	ca := testca.New(t)
	certFile, keyFile := ca.WriteLeafFiles(t, dir, "proxy", []string{"localhost"}, nil)

	tests := []struct {
		name     string
		certFile string
		keyFile  string
		want     string
	}{
		{"missing cert", filepath.Join(dir, "absent.crt"), keyFile, "read TLS cert"},
		{"missing key", certFile, filepath.Join(dir, "absent.key"), "read TLS key"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, "https://upstream:8443", ca.WriteCertFile(t, dir))
			cfg.TLSCertFile, cfg.TLSKeyFile = tc.certFile, tc.keyFile
			cfg.ListenAddr = freeAddr(t)

			err := Start(context.Background(), cfg, http.NotFoundHandler())
			if err == nil {
				t.Fatalf("Start succeeded, want error mentioning %q", tc.want)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tc.want)) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
