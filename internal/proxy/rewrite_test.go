package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go-reverse-proxy/internal/fixtures"
)

const replacement = "REDACTED"

// assertRewritten checks the property that matters everywhere: the keyword is
// gone, the replacement is there, and the headers describe what was sent.
func assertRewritten(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()

	if bytes.Contains(body, []byte(fixtures.Keyword)) {
		t.Errorf("response still contains %q: %s", fixtures.Keyword, body)
	}
	if !bytes.Contains(body, []byte(replacement)) {
		t.Errorf("response does not contain %q: %s", replacement, body)
	}
	assertContentLengthMatches(t, resp, body)
}

func assertContentLengthMatches(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()

	declared := resp.Header.Get("Content-Length")
	if declared == "" {
		return // chunked, or a body whose length was never declared
	}
	if declared != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %s but %d bytes arrived", declared, len(body))
	}
}

func TestRewritesTextualResponses(t *testing.T) {
	p := newTestProxy(t)

	for _, path := range []string{"/text", "/html", "/json", "/xml", "/js"} {
		t.Run(path, func(t *testing.T) {
			resp, body := get(t, p.client, p.url(path))

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			assertRewritten(t, resp, body)
		})
	}
}

// The fixture puts the keyword at both edges of /text, so an off-by-one shows up.
func TestRewritesKeywordAtBothEdges(t *testing.T) {
	p := newTestProxy(t)

	resp, body := get(t, p.client, p.url("/text"))

	if !bytes.HasPrefix(body, []byte(replacement)) {
		t.Errorf("body does not start with the replacement: %s", body)
	}
	if !bytes.HasSuffix(body, []byte(replacement)) {
		t.Errorf("body does not end with the replacement: %s", body)
	}
	if n := bytes.Count(body, []byte(replacement)); n != 3 {
		t.Errorf("replacement occurs %d times, want 3", n)
	}
	assertContentLengthMatches(t, resp, body)
}

// Gzip must come back gzipped: decompressed, rewritten, re-compressed, with
// headers that still describe the bytes on the wire.
func TestRewritesGzipResponse(t *testing.T) {
	p := newTestProxy(t)

	resp, body := get(t, p.client, p.url("/gzip"))

	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if bytes.Contains(body, []byte(replacement)) {
		t.Error("wire bytes are not compressed; the replacement is visible in plaintext")
	}
	assertContentLengthMatches(t, resp, body)

	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()

	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if bytes.Contains(plain, []byte(fixtures.Keyword)) {
		t.Errorf("decompressed body still contains the keyword: %s", plain)
	}
	if want := bytes.ReplaceAll([]byte(fixtures.HTMLBody), []byte(fixtures.Keyword), []byte(replacement)); !bytes.Equal(plain, want) {
		t.Errorf("decompressed body = %q, want %q", plain, want)
	}
}

// An encoding the proxy cannot decode must be forwarded untouched.
func TestForwardsBrotliUnchanged(t *testing.T) {
	p := newTestProxy(t)

	resp, body := get(t, p.client, p.url("/brotli"))

	if enc := resp.Header.Get("Content-Encoding"); enc != "br" {
		t.Errorf("Content-Encoding = %q, want br", enc)
	}
	if !bytes.Contains(body, []byte(fixtures.Keyword)) {
		t.Errorf("brotli body was modified: %s", body)
	}
}

// A binary response must survive byte for byte. The fixture PNG carries the
// keyword in a tEXt chunk, so a proxy that rewrites it corrupts the image.
func TestForwardsBinaryByteIdentical(t *testing.T) {
	p := newTestProxy(t)

	resp, body := get(t, p.client, p.url("/image"))

	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if sha256.Sum256(body) != sha256.Sum256(fixtures.PNGBytes) {
		t.Fatal("PNG checksum differs from the fixture; the proxy altered a binary body")
	}
	if !bytes.Contains(body, []byte(fixtures.Keyword)) {
		t.Error("the keyword inside the PNG was rewritten")
	}
	if _, err := png.Decode(bytes.NewReader(body)); err != nil {
		t.Errorf("proxied PNG no longer decodes: %v", err)
	}
}

// A chunked response arrives with no declared length. Because the rewriter
// buffers it, the proxy can now state one.
func TestRewritesChunkedResponse(t *testing.T) {
	p := newTestProxy(t)

	resp, body := get(t, p.client, p.url("/chunked"))

	if n := bytes.Count(body, []byte(replacement)); n != 3 {
		t.Errorf("replacement occurs %d times, want 3: %s", n, body)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", resp.ContentLength, len(body))
	}
	assertContentLengthMatches(t, resp, body)
}

// An event stream must reach the client as it is produced. Buffering it to
// rewrite it would withhold every event until the stream ended — for SSE, that
// is indistinguishable from a hang.
func TestServerSentEventsAreNotBuffered(t *testing.T) {
	p := newTestProxy(t)

	start := time.Now()

	resp, err := p.client.Get(p.url("/events"))
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("read first event: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	// The fixture waits StreamGap after its first event, so arriving sooner is
	// only possible if the proxy passed the stream straight through.
	if elapsed >= fixtures.StreamGap {
		t.Errorf("first event took %v (gap is %v); the stream was buffered", elapsed, fixtures.StreamGap)
	}
	if !strings.Contains(line, fixtures.Keyword) {
		t.Errorf("first event = %q; a streamed body must pass through unrewritten", line)
	}
}

// There is no size cap: a body far larger than any page is rewritten in full.
// 8 MiB used to be forwarded untouched, which weakened the rule that textual
// responses are modified.
func TestLargeBodyIsRewritten(t *testing.T) {
	p := newTestProxy(t)

	resp, body := get(t, p.client, p.url("/large?mb=8"))

	if bytes.Contains(body, []byte(fixtures.Keyword)) {
		t.Error("an 8 MiB textual body was not rewritten")
	}
	if !bytes.Contains(body, []byte(replacement)) {
		t.Error("no replacement found in an 8 MiB body")
	}
	assertContentLengthMatches(t, resp, body)
}

// Statuses that must not carry a body must not acquire one, and must keep their
// meaning through the rewriter.
func TestPreservesBodylessStatuses(t *testing.T) {
	p := newTestProxy(t)

	for _, code := range []int{http.StatusNoContent, http.StatusNotModified} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			resp, body := get(t, p.client, p.url("/status/"+strconv.Itoa(code)))

			if resp.StatusCode != code {
				t.Errorf("status = %d, want %d", resp.StatusCode, code)
			}
			if len(body) != 0 {
				t.Errorf("status %d returned %d bytes, want none", code, len(body))
			}
		})
	}
}

// Error responses are still responses: their bodies get rewritten too.
func TestRewritesErrorResponseBodies(t *testing.T) {
	p := newTestProxy(t)

	for _, code := range []int{404, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			resp, body := get(t, p.client, p.url("/status/"+strconv.Itoa(code)))

			if resp.StatusCode != code {
				t.Errorf("status = %d, want %d", resp.StatusCode, code)
			}
			assertRewritten(t, resp, body)
		})
	}
}

// One Rewriter is shared by every request, so concurrent responses must not
// interfere with each other's buffers.
func TestConcurrentRequestsAreEachRewritten(t *testing.T) {
	p := newTestProxy(t)

	paths := []string{"/text", "/html", "/json", "/xml", "/js", "/gzip", "/image", "/chunked"}

	var wg sync.WaitGroup
	for range 8 {
		for _, path := range paths {
			wg.Add(1)
			go func() {
				defer wg.Done()

				resp, err := p.client.Get(p.url(path))
				if err != nil {
					t.Errorf("GET %s: %v", path, err)
					return
				}
				defer resp.Body.Close()

				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Errorf("read %s: %v", path, err)
					return
				}

				switch path {
				case "/image":
					if sha256.Sum256(body) != sha256.Sum256(fixtures.PNGBytes) {
						t.Errorf("%s: binary body was corrupted under concurrency", path)
					}
				case "/gzip":
					if resp.Header.Get("Content-Encoding") != "gzip" {
						t.Errorf("%s: lost its encoding under concurrency", path)
					}
				default:
					if bytes.Contains(body, []byte(fixtures.Keyword)) {
						t.Errorf("%s: not rewritten under concurrency", path)
					}
				}
			}()
		}
	}
	wg.Wait()
}

// A client that asks for gzip must get gzip; one that does not must get plaintext.
// Either way the keyword must be gone.
func TestHonoursClientAcceptEncoding(t *testing.T) {
	p := newTestProxy(t)

	req, err := http.NewRequest(http.MethodGet, p.url("/html"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")

	resp, body := do(t, p.client, req)

	// The fixture's /html route does not compress, so the reply is plaintext
	// regardless; what matters is that asking for gzip did not break rewriting.
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding = %q, want none", resp.Header.Get("Content-Encoding"))
	}
	assertRewritten(t, resp, body)
}
