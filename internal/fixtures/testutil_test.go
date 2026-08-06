package fixtures

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-reverse-proxy/internal/testca"
)

// loopbackSANs covers the addresses httptest binds to.
func loopbackSANs() ([]string, []net.IP) {
	return []string{"localhost", "upstream"}, []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
}

// newHTTPTestServer serves the fixtures over plaintext HTTP.
//
// DisableCompression matters in both modes: Go's transport otherwise adds its
// own Accept-Encoding: gzip, transparently decompresses the reply, and strips
// the Content-Encoding header — hiding exactly what these tests assert.
func newHTTPTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()

	srv := httptest.NewServer(NewServer())
	t.Cleanup(srv.Close)

	return srv, &http.Client{
		Transport:     &http.Transport{DisableCompression: true},
		CheckRedirect: noRedirect,
	}
}

// newTLSTestServer serves the fixtures over HTTPS using a CA-signed leaf, and
// returns a client that trusts only that CA. This exercises real chain building
// and hostname verification rather than httptest's self-trusting shortcut.
func newTLSTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()

	ca := testca.New(t)
	dnsNames, ips := loopbackSANs()

	srv := httptest.NewUnstartedServer(NewServer())
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.Leaf(t, "upstream", dnsNames, ips)},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv, newTLSClient(ca.Pool)
}

func newTLSClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
			ForceAttemptHTTP2:  true,
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
		CheckRedirect: noRedirect,
	}
}

// noRedirect returns redirects to the caller instead of following them.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

type serverMode struct {
	name  string
	start func(t *testing.T) (*httptest.Server, *http.Client)
}

// eachMode runs fn against the fixture server over both HTTP and HTTPS, so no
// fixture can regress on one transport while passing on the other.
func eachMode(t *testing.T, fn func(t *testing.T, srv *httptest.Server, client *http.Client)) {
	t.Helper()

	for _, mode := range []serverMode{
		{"http", newHTTPTestServer},
		{"https", newTLSTestServer},
	} {
		t.Run(mode.name, func(t *testing.T) {
			srv, client := mode.start(t)
			fn(t, srv, client)
		})
	}
}

func get(t *testing.T, client *http.Client, url string) (*http.Response, []byte) {
	t.Helper()

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", url, err)
	}
	return resp, body
}
