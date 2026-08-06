package fixtures

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-reverse-proxy/internal/testca"
)

// A client that does not hold the CA must be refused. If this ever passes, the
// trust chain has been bypassed somewhere — an empty RootCAs pool, or an
// InsecureSkipVerify that crept in.
func TestTLSRejectsClientWithoutCA(t *testing.T) {
	srv, _ := newTLSTestServer(t)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    x509.NewCertPool(), // deliberately empty
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	resp, err := client.Get(srv.URL + "/json")
	if err == nil {
		resp.Body.Close()
		t.Fatal("request succeeded without the CA; certificate verification is not being enforced")
	}

	var unknownAuthority x509.UnknownAuthorityError
	if !errors.As(err, &unknownAuthority) {
		t.Logf("got error %v", err)
	}
}

// The system root store must not accept our private CA either.
func TestTLSRejectsSystemRoots(t *testing.T) {
	srv, _ := newTLSTestServer(t)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, // RootCAs nil => system roots
		},
	}

	resp, err := client.Get(srv.URL + "/json")
	if err == nil {
		resp.Body.Close()
		t.Fatal("private CA was accepted by the system root store")
	}
}

// A certificate without a matching SAN must fail hostname verification even
// though the chain itself is valid. This is the failure mode that bites when a
// Compose service name is not listed in certgen.sh.
func TestTLSRejectsCertificateWithWrongSAN(t *testing.T) {
	ca := testca.New(t)

	srv := httptest.NewUnstartedServer(NewServer())
	srv.TLS = &tls.Config{
		// Valid cert, correct CA — but no SAN for 127.0.0.1 or localhost.
		Certificates: []tls.Certificate{ca.Leaf(t, "upstream", []string{"somewhere-else.invalid"}, nil)},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	resp, err := newTLSClient(ca.Pool).Get(srv.URL + "/json")
	if err == nil {
		resp.Body.Close()
		t.Fatal("hostname verification did not reject a certificate with no matching SAN")
	}

	var hostErr x509.HostnameError
	if !errors.As(err, &hostErr) {
		t.Errorf("want x509.HostnameError, got %v", err)
	}
}

// The leaf the CA issues must cover every name the deployment dials.
func TestLeafCertificateCoversExpectedSANs(t *testing.T) {
	ca := testca.New(t)
	dnsNames, ips := loopbackSANs()

	pair := ca.Leaf(t, "upstream", dnsNames, ips)
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	for _, name := range []string{"localhost", "upstream"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: ca.Pool}); err != nil {
			t.Errorf("leaf is not valid for %q: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "proxy", Roots: ca.Pool}); err == nil {
		t.Error("leaf unexpectedly valid for \"proxy\"; SAN list is too broad")
	}

	if !leaf.IPAddresses[0].Equal(net.IPv4(127, 0, 0, 1)) {
		t.Errorf("IP SANs = %v, want 127.0.0.1 first", leaf.IPAddresses)
	}
}

// The upstream advertises h2 and http/1.1. If ALPN ever narrows to h2 only, a
// proxy with a custom TLSClientConfig fails the handshake outright.
func TestTLSNegotiatesHTTP2(t *testing.T) {
	srv, client := newTLSTestServer(t)

	resp, _ := get(t, client, srv.URL+"/json")
	if resp.ProtoMajor != 2 {
		t.Errorf("negotiated HTTP/%d.%d, want HTTP/2", resp.ProtoMajor, resp.ProtoMinor)
	}
}

// An HTTP/1.1-only client must still be served, which is what listing
// http/1.1 in NextProtos buys.
func TestTLSStillServesHTTP11Clients(t *testing.T) {
	ca := testca.New(t)
	dnsNames, ips := loopbackSANs()

	srv := httptest.NewUnstartedServer(NewServer())
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.Leaf(t, "upstream", dnsNames, ips)},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
			ForceAttemptHTTP2:  false,
			TLSClientConfig: &tls.Config{
				RootCAs:    ca.Pool,
				MinVersion: tls.VersionTLS12,
				NextProtos: []string{"http/1.1"},
			},
		},
	}

	resp, err := client.Get(srv.URL + "/json")
	if err != nil {
		t.Fatalf("HTTP/1.1 client rejected: %v", err)
	}
	defer resp.Body.Close()

	if resp.ProtoMajor != 1 {
		t.Errorf("negotiated HTTP/%d, want HTTP/1.1", resp.ProtoMajor)
	}
}

// TLS 1.0/1.1 must be refused. The client trusts the CA here, so protocol
// version is the only reason the handshake can fail.
func TestTLSRejectsObsoleteVersions(t *testing.T) {
	ca := testca.New(t)
	dnsNames, ips := loopbackSANs()

	srv := httptest.NewUnstartedServer(NewServer())
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.Leaf(t, "upstream", dnsNames, ips)},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// Sanity check: the same CA over a modern version must succeed, otherwise a
	// failure below proves nothing.
	if resp, err := newTLSClient(ca.Pool).Get(srv.URL + "/json"); err != nil {
		t.Fatalf("baseline TLS 1.2+ request failed: %v", err)
	} else {
		resp.Body.Close()
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    ca.Pool,
				MinVersion: tls.VersionTLS10,
				MaxVersion: tls.VersionTLS11,
			},
		},
	}

	resp, err := client.Get(srv.URL + "/json")
	if err == nil {
		resp.Body.Close()
		t.Fatal("server accepted a TLS 1.1 client")
	}
}
