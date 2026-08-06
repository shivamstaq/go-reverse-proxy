package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go-reverse-proxy/internal/config"
	"go-reverse-proxy/internal/fixtures"
	"go-reverse-proxy/internal/testca"
)

// testProxy is the real fixture origin, over real TLS with a real chain, behind
// the real proxy handler. The client-facing hop is plain HTTP: terminating
// client TLS is Start's job and is exercised separately.
type testProxy struct {
	front        *httptest.Server // what the client talks to
	client       *http.Client
	clientHost   string // authority the client addresses
	upstreamHost string // authority the upstream is reachable at
}

func newTestProxy(t *testing.T) testProxy {
	t.Helper()

	ca := testca.New(t)

	upstream := httptest.NewUnstartedServer(fixtures.NewServer())
	upstream.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.Leaf(t, "upstream",
			[]string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback})},
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
	}
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	cfg := testConfig(t, upstream.URL, ca.WriteCertFile(t, t.TempDir()))

	handler, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	front := httptest.NewServer(handler)
	t.Cleanup(front.Close)

	return testProxy{
		front:        front,
		client:       newClient(),
		clientHost:   authorityOf(t, front.URL),
		upstreamHost: cfg.Upstream.Host,
	}
}

func (p testProxy) url(path string) string { return p.front.URL + path }

func testConfig(t *testing.T, upstreamURL, caFile string) config.Config {
	t.Helper()

	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	return config.Config{
		ListenAddr:  "127.0.0.1:0",
		Upstream:    u,
		CACertFile:  caFile,
		Keyword:     fixtures.Keyword,
		Replacement: "REDACTED",
	}
}

func authorityOf(t *testing.T, rawURL string) string {
	t.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Host
}

// DisableCompression keeps Go's transport from adding its own Accept-Encoding
// and silently decompressing, which would hide what the proxy actually sent.
func newClient() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func get(t *testing.T, client *http.Client, url string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return do(t, client, req)
}

func do(t *testing.T, client *http.Client, req *http.Request) (*http.Response, []byte) {
	t.Helper()

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", req.URL, err)
	}
	return resp, body
}

// echoResponse is what the fixture /echo route reports back about the request
// the upstream actually received.
type echoResponse struct {
	Method  string              `json:"method"`
	Host    string              `json:"host"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

func decodeEcho(t *testing.T, body []byte) echoResponse {
	t.Helper()

	var got echoResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode /echo response %q: %v", body, err)
	}
	return got
}

func TestForwardsToUpstream(t *testing.T) {
	p := newTestProxy(t)

	resp, body := get(t, p.client, p.url("/html"))

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("<h1>")) {
		t.Errorf("body does not look like the upstream document: %s", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// The upstream must see its own authority, so a virtual-hosted origin can route
// the request. Echo's proxy middleware alone forwards the client's Host.
func TestRewritesOutgoingHostHeader(t *testing.T) {
	p := newTestProxy(t)

	_, body := get(t, p.client, p.url("/echo"))
	got := decodeEcho(t, body)

	if got.Host == p.clientHost {
		t.Fatalf("upstream saw the client's Host %q; it was not rewritten", got.Host)
	}
	if got.Host != p.upstreamHost {
		t.Errorf("upstream saw Host %q, want %q", got.Host, p.upstreamHost)
	}
	if fwd := got.Headers["X-Forwarded-Host"]; len(fwd) != 1 || fwd[0] != p.clientHost {
		t.Errorf("X-Forwarded-Host = %v, want [%s]", fwd, p.clientHost)
	}
}

// An existing X-Forwarded-Host chain from an edge proxy must survive.
func TestPreservesExistingForwardedHost(t *testing.T) {
	p := newTestProxy(t)

	req, err := http.NewRequest(http.MethodGet, p.url("/echo"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Host", "edge.example.com")

	_, body := do(t, p.client, req)
	got := decodeEcho(t, body)

	if fwd := got.Headers["X-Forwarded-Host"]; len(fwd) != 1 || fwd[0] != "edge.example.com" {
		t.Errorf("X-Forwarded-Host = %v, want it preserved", fwd)
	}
	if got.Host != p.upstreamHost {
		t.Errorf("Host = %q, want it still rewritten to %q", got.Host, p.upstreamHost)
	}
}

// This proxy terminates TLS with its own certificate rather than tunnelling, so
// a CONNECT must be refused explicitly instead of being relayed upstream.
func TestRejectsConnect(t *testing.T) {
	p := newTestProxy(t)

	req, err := http.NewRequest(http.MethodConnect, p.url("/"), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, _ := do(t, p.client, req)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

// A HEAD response describes a body that is not there; it must pass through with
// its status and headers intact and no body invented.
func TestForwardsHeadWithoutBody(t *testing.T) {
	p := newTestProxy(t)

	req, err := http.NewRequest(http.MethodHead, p.url("/html"), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, body := do(t, p.client, req)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Errorf("HEAD returned %d bytes, want none", len(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	// A HEAD stands in for a GET, and the GET's body is rewritten to a
	// different length, so claiming the upstream's length would misinform.
	if got := resp.Header.Get("Content-Length"); got != "" {
		_, getBody := get(t, p.client, p.url("/html"))
		t.Errorf("HEAD declares Content-Length %s but a GET returns %d bytes", got, len(getBody))
	}
}

// The query string is part of the request and must reach the upstream intact.
func TestForwardsQueryString(t *testing.T) {
	p := newTestProxy(t)

	// /large honours ?mb=, so a preserved query is visible in the body size.
	_, body := get(t, p.client, p.url("/large?mb=1"))

	if len(body) == 0 {
		t.Fatal("empty body; the query string did not reach the upstream")
	}
	// One rewritten megabyte: shorter than the original, but the same order.
	if len(body) > fixtures.BlockSize || len(body) < fixtures.BlockSize/2 {
		t.Errorf("body is %d bytes, want roughly %d", len(body), fixtures.BlockSize)
	}
}

// The access log reads Host after the handler returns. Rewriting it for the
// upstream must not leave the log claiming the client asked for the upstream.
func TestAccessLogSeesClientHost(t *testing.T) {
	var logged bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	p := newTestProxy(t)
	get(t, p.client, p.url("/html"))

	if !bytes.Contains(logged.Bytes(), []byte(`"host":"`+p.clientHost+`"`)) {
		t.Errorf("access log does not report the client Host %q:\n%s", p.clientHost, logged.String())
	}
	if bytes.Contains(logged.Bytes(), []byte(`"host":"`+p.upstreamHost+`"`)) {
		t.Errorf("access log reports the upstream Host %q as if the client sent it", p.upstreamHost)
	}
}

// An empty request_id on every line makes correlating a client report with a log
// impossible, so the middleware that populates it must be wired in.
func TestAccessLogCarriesRequestID(t *testing.T) {
	var logged bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	p := newTestProxy(t)
	get(t, p.client, p.url("/html"))

	if bytes.Contains(logged.Bytes(), []byte(`"request_id":""`)) {
		t.Errorf("access log has an empty request_id:\n%s", logged.String())
	}
}

func TestPreservesStatusCodes(t *testing.T) {
	p := newTestProxy(t)

	for _, code := range []int{201, 404, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			resp, _ := get(t, p.client, p.url("/status/"+strconv.Itoa(code)))
			if resp.StatusCode != code {
				t.Errorf("status = %d, want %d", resp.StatusCode, code)
			}
		})
	}
}

func TestPreservesResponseHeadersAndCookies(t *testing.T) {
	p := newTestProxy(t)

	resp, _ := get(t, p.client, p.url("/headers"))

	if got := resp.Header.Get("X-Custom-Header"); got != "custom-"+fixtures.Keyword {
		t.Errorf("X-Custom-Header = %q", got)
	}
	if cookies := resp.Cookies(); len(cookies) != 1 || cookies[0].Name != "session" {
		t.Errorf("cookies = %v, want one named session", cookies)
	}
}

func TestPreservesRedirectsWithoutFollowing(t *testing.T) {
	p := newTestProxy(t)

	resp, _ := get(t, p.client, p.url("/redirect"))

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/html" {
		t.Errorf("Location = %q, want /html", loc)
	}
}

func TestForwardsMethodAndRequestBody(t *testing.T) {
	p := newTestProxy(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			// The payload deliberately avoids the keyword: this test is about
			// forwarding, and /echo's reply is itself subject to rewriting.
			req, err := http.NewRequest(method, p.url("/echo"), strings.NewReader("payload-body"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Test-Header", "sentinel")

			_, body := do(t, p.client, req)
			got := decodeEcho(t, body)

			if got.Method != method {
				t.Errorf("method = %q, want %q", got.Method, method)
			}
			if got.Body != "payload-body" {
				t.Errorf("body = %q", got.Body)
			}
			if h := got.Headers["X-Test-Header"]; len(h) != 1 || h[0] != "sentinel" {
				t.Errorf("X-Test-Header = %v", h)
			}
		})
	}
}

// An unreachable upstream must be reported as a gateway error — not a panic, not
// a hang — and must not leak the upstream address to the client.
func TestUnreachableUpstreamReturns502(t *testing.T) {
	ca := testca.New(t)

	// Port 1 on loopback: nothing listens there.
	cfg := testConfig(t, "https://127.0.0.1:1", ca.WriteCertFile(t, t.TempDir()))

	handler, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	front := httptest.NewServer(handler)
	t.Cleanup(front.Close)

	resp, body := get(t, newClient(), front.URL+"/html")

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if bytes.Contains(body, []byte("127.0.0.1:1")) {
		t.Errorf("response leaks the upstream address: %s", body)
	}
}

// An upstream the CA does not vouch for must be refused, not trusted.
func TestRejectsUntrustedUpstreamCertificate(t *testing.T) {
	other := testca.New(t)

	upstream := httptest.NewUnstartedServer(fixtures.NewServer())
	upstream.TLS = &tls.Config{
		Certificates: []tls.Certificate{other.Leaf(t, "upstream",
			[]string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback})},
		MinVersion: tls.VersionTLS12,
	}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	// The proxy trusts a different CA than the one that signed the upstream.
	ours := testca.New(t)
	handler, err := NewHandler(testConfig(t, upstream.URL, ours.WriteCertFile(t, t.TempDir())))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	front := httptest.NewServer(handler)
	t.Cleanup(front.Close)

	resp, _ := get(t, newClient(), front.URL+"/html")

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; the proxy trusted an unverified upstream", resp.StatusCode)
	}
}

func TestNewHandlerRejectsBadCAFile(t *testing.T) {
	dir := t.TempDir()

	notPEM := filepath.Join(dir, "not-a-cert.pem")
	if err := os.WriteFile(notPEM, []byte("this is not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		caFile string
		want   string
	}{
		{"missing", filepath.Join(dir, "absent.crt"), "read CA cert"},
		{"not a certificate", notPEM, "no certificates found"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewHandler(testConfig(t, "https://upstream:8443", tc.caFile))
			if err == nil {
				t.Fatalf("NewHandler succeeded, want error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
