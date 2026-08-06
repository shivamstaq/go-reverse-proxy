package fixtures

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestTextualFixtures(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		tests := []struct {
			path        string
			contentType string
		}{
			{"/text", "text/plain"},
			{"/html", "text/html"},
			{"/json", "application/json"},
			{"/xml", "application/xml"},
			{"/js", "application/javascript"},
		}

		for _, tc := range tests {
			t.Run(tc.path, func(t *testing.T) {
				resp, body := get(t, client, srv.URL+tc.path)

				if resp.StatusCode != http.StatusOK {
					t.Errorf("status = %d, want 200", resp.StatusCode)
				}
				if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
					t.Errorf("Content-Type = %q, want prefix %q", ct, tc.contentType)
				}
				if !bytes.Contains(body, []byte(Keyword)) {
					t.Errorf("body does not contain %q: %s", Keyword, body)
				}
			})
		}
	})
}

// The keyword must sit at both edges of /text so an off-by-one in the proxy's
// replacement is visible.
func TestTextKeywordAtBothEdges(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		_, body := get(t, client, srv.URL+"/text")

		if !bytes.HasPrefix(body, []byte(Keyword)) {
			t.Errorf("body does not start with %q: %s", Keyword, body)
		}
		if !bytes.HasSuffix(body, []byte(Keyword)) {
			t.Errorf("body does not end with %q: %s", Keyword, body)
		}
		if n := bytes.Count(body, []byte(Keyword)); n != 3 {
			t.Errorf("keyword occurs %d times, want 3", n)
		}
	})
}

func TestGzipFixture(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		resp, body := get(t, client, srv.URL+"/gzip")

		if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", enc)
		}
		// The wire bytes must be compressed, not plaintext.
		if bytes.Contains(body, []byte(Keyword)) {
			t.Error("compressed body contains the keyword in plaintext; it is not actually gzipped")
		}

		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		defer zr.Close()

		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("decompress: %v", err)
		}
		if string(plain) != HTMLBody {
			t.Error("decompressed body does not match /html")
		}
	})
}

func TestBrotliFixturePassesThroughUndecodable(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		resp, body := get(t, client, srv.URL+"/brotli")

		if enc := resp.Header.Get("Content-Encoding"); enc != "br" {
			t.Errorf("Content-Encoding = %q, want br", enc)
		}
		if !bytes.Contains(body, []byte(Keyword)) {
			t.Errorf("body = %q, want it to contain %q", body, Keyword)
		}
	})
}

func TestImageFixtureIsValidPNGContainingKeyword(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		resp, body := get(t, client, srv.URL+"/image")

		if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
			t.Errorf("Content-Type = %q, want image/png", ct)
		}
		if !bytes.Contains(body, []byte(Keyword)) {
			t.Fatalf("PNG does not contain %q as literal bytes", Keyword)
		}
		if _, err := png.Decode(bytes.NewReader(body)); err != nil {
			t.Fatalf("fixture is not a decodable PNG: %v", err)
		}
		// Byte-identical to what the handler built, on both transports.
		if !bytes.Equal(body, PNGBytes) {
			t.Error("served PNG differs from the fixture bytes")
		}
	})
}

// Rewriting the keyword inside the PNG must break decoding. If this ever passes
// silently, the binary fixture has stopped being a useful tripwire.
func TestImageCorruptionIsDetectable(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		_, body := get(t, client, srv.URL+"/image")

		tampered := bytes.ReplaceAll(body, []byte(Keyword), []byte("REPLACED-WITH-LONGER-TEXT"))
		if bytes.Equal(tampered, body) {
			t.Fatal("tampering changed nothing")
		}
		if sha256.Sum256(tampered) == sha256.Sum256(body) {
			t.Fatal("checksums match after tampering")
		}
		if _, err := png.Decode(bytes.NewReader(tampered)); err == nil {
			t.Error("tampered PNG still decodes; corruption would go unnoticed")
		}
	})
}

func TestLargeFixtureSize(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		_, body := get(t, client, srv.URL+"/large?mb=2")

		if want := 2 * BlockSize; len(body) != want {
			t.Errorf("len(body) = %d, want %d", len(body), want)
		}
		if !bytes.Contains(body, []byte(Keyword)) {
			t.Error("large body does not contain the keyword")
		}
	})
}

func TestLargeFixtureRejectsBadSize(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		resp, _ := get(t, client, srv.URL+"/large?mb=abc")

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

// Over HTTP/1.1 this manifests as Transfer-Encoding: chunked. HTTP/2 has no
// chunked encoding — it frames the body instead — so the portable assertion is
// "the length was not known up front".
func TestChunkedFixtureHasNoContentLength(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		resp, body := get(t, client, srv.URL+"/chunked")

		if resp.ContentLength != -1 {
			t.Errorf("ContentLength = %d, want -1 (unknown)", resp.ContentLength)
		}
		if resp.ProtoMajor == 1 {
			if len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked" {
				t.Errorf("TransferEncoding = %v, want [chunked]", resp.TransferEncoding)
			}
		}
		if n := bytes.Count(body, []byte(Keyword)); n != 3 {
			t.Errorf("keyword occurs %d times, want 3", n)
		}
	})
}

func TestStatusFixture(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		for _, code := range []int{201, 404, 500} {
			t.Run(strconv.Itoa(code), func(t *testing.T) {
				resp, body := get(t, client, srv.URL+"/status/"+strconv.Itoa(code))

				if resp.StatusCode != code {
					t.Errorf("status = %d, want %d", resp.StatusCode, code)
				}
				if !bytes.Contains(body, []byte(Keyword)) {
					t.Errorf("body %q does not contain the keyword", body)
				}
			})
		}
	})
}

func TestStatusFixtureNoBodyStatuses(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		for _, code := range []int{204, 304} {
			resp, body := get(t, client, srv.URL+"/status/"+strconv.Itoa(code))

			if resp.StatusCode != code {
				t.Errorf("status = %d, want %d", resp.StatusCode, code)
			}
			if len(body) != 0 {
				t.Errorf("status %d returned a %d-byte body, want empty", code, len(body))
			}
		}
	})
}

func TestStatusFixtureRejectsGarbage(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		resp, _ := get(t, client, srv.URL+"/status/999")

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestRedirectFixture(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		resp, _ := get(t, client, srv.URL+"/redirect")

		if resp.StatusCode != http.StatusFound {
			t.Errorf("status = %d, want 302", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/html" {
			t.Errorf("Location = %q, want /html", loc)
		}
	})
}

func TestHeadersFixture(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		resp, _ := get(t, client, srv.URL+"/headers")

		if got := resp.Header.Get("X-Custom-Header"); got != "custom-"+Keyword {
			t.Errorf("X-Custom-Header = %q", got)
		}
		if cookies := resp.Cookies(); len(cookies) != 1 || cookies[0].Name != "session" {
			t.Errorf("cookies = %v, want one named session", cookies)
		}
	})
}

func TestEchoFixtureRoundTripsMethodAndBody(t *testing.T) {
	eachMode(t, func(t *testing.T, srv *httptest.Server, client *http.Client) {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			t.Run(method, func(t *testing.T) {
				req, err := http.NewRequest(method, srv.URL+"/echo", strings.NewReader("payload-"+Keyword))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Test-Header", "sentinel")

				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("%s /echo: %v", method, err)
				}
				defer resp.Body.Close()

				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{method, "payload-" + Keyword, "sentinel"} {
					if !bytes.Contains(body, []byte(want)) {
						t.Errorf("echo response missing %q: %s", want, body)
					}
				}
				// Host must be reported so R10 is observable through the proxy.
				if !bytes.Contains(body, []byte(`"host"`)) {
					t.Errorf("echo response does not report Host: %s", body)
				}
			})
		}
	})
}
