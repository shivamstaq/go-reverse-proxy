package rewriter

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

const (
	keyword     = "test-keyword"
	replacement = "REDACTED"
	testLimit   = 1 << 20
)

func newRewriter(t *testing.T) *Rewriter {
	t.Helper()

	r, err := New(keyword, replacement)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// trackedBody records whether the response body was closed. Failing to close a
// body that has been read to EOF leaks the upstream connection.
type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

type responseOption func(*http.Response)

func withHeader(key, value string) responseOption {
	return func(res *http.Response) { res.Header.Set(key, value) }
}

func withStatus(code int) responseOption {
	return func(res *http.Response) { res.StatusCode = code }
}

func withMethod(method string) responseOption {
	return func(res *http.Response) { res.Request.Method = method }
}

// newResponse builds a response as the upstream transport would hand it over.
func newResponse(body []byte, contentType string, opts ...responseOption) (*http.Response, *trackedBody) {
	tracked := &trackedBody{Reader: bytes.NewReader(body)}

	res := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{},
		Body:          tracked,
		ContentLength: int64(len(body)),
		Request:       &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/test"}},
	}
	if contentType != "" {
		res.Header.Set("Content-Type", contentType)
	}
	res.Header.Set("Content-Length", strconv.Itoa(len(body)))

	for _, opt := range opts {
		opt(res)
	}
	return res, tracked
}

func readBody(t *testing.T, res *http.Response) []byte {
	t.Helper()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read rewritten body: %v", err)
	}
	return body
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()

	out, err := gzipEncode(b)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	return out
}

func gunzipAll(t *testing.T, b []byte) []byte {
	t.Helper()

	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()

	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	return out
}

// --- what must be rewritten ---

func TestRewritesTextualTypes(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
	}{
		{"plain text", "text/plain; charset=utf-8"},
		{"html", "text/html; charset=utf-8"},
		{"css", "text/css"},
		{"json", "application/json"},
		{"json with charset", "application/json; charset=UTF-8"},
		{"xml", "application/xml; charset=utf-8"},
		{"javascript", "application/javascript; charset=utf-8"},
		{"legacy javascript", "application/x-javascript"},
		{"xhtml", "application/xhtml+xml"},
		{"json suffix", "application/ld+json"},
		{"xml suffix", "application/atom+xml"},
		{"us-ascii", "text/plain; charset=us-ascii"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := newResponse([]byte("a "+keyword+" here"), tc.contentType)

			if err := newRewriter(t).Apply(res); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := string(readBody(t, res)); got != "a "+replacement+" here" {
				t.Errorf("body = %q", got)
			}
		})
	}
}

// The keyword at both edges catches an off-by-one in the replacement.
func TestRewritesEveryOccurrence(t *testing.T) {
	body := keyword + " middle " + keyword + " end " + keyword
	res, _ := newResponse([]byte(body), "text/plain")

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := readBody(t, res)
	if bytes.Contains(got, []byte(keyword)) {
		t.Errorf("body still contains the keyword: %s", got)
	}
	if n := bytes.Count(got, []byte(replacement)); n != 3 {
		t.Errorf("replacement occurs %d times, want 3", n)
	}
}

// Replacement is literal and case-sensitive: a different case is a different word.
//
// The variants are derived from the keyword rather than written out, so renaming
// the keyword cannot silently turn this into a test that feeds the keyword itself
// and then asserts it survives.
func TestReplacementIsCaseSensitive(t *testing.T) {
	body := strings.ToUpper(keyword) + " " + strings.ToUpper(keyword[:1]) + keyword[1:]

	// Fail loudly rather than pass vacuously if the keyword has no case to vary.
	if strings.Contains(body, keyword) {
		t.Fatalf("keyword %q has no distinguishable case variants: %q", keyword, body)
	}

	res, _ := newResponse([]byte(body), "text/plain")

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := string(readBody(t, res)); got != body {
		t.Errorf("body = %q, want %q untouched", got, body)
	}
}

// --- headers must describe the body actually sent ---

func TestUpdatesHeadersAfterRewrite(t *testing.T) {
	res, _ := newResponse([]byte(keyword), "text/plain",
		withHeader("Etag", `"abc123"`),
		withHeader("Content-Md5", "deadbeef"),
		withHeader("Digest", "sha-256=xyz"),
	)

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	body := readBody(t, res)
	if want := strconv.Itoa(len(body)); res.Header.Get("Content-Length") != want {
		t.Errorf("Content-Length = %q, want %q", res.Header.Get("Content-Length"), want)
	}
	if res.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", res.ContentLength, len(body))
	}
	// The body changed, so validators describing the old bytes must go.
	for _, header := range []string{"Etag", "Content-Md5", "Digest"} {
		if got := res.Header.Get(header); got != "" {
			t.Errorf("%s = %q, want it removed", header, got)
		}
	}
}

// A shorter replacement must shrink Content-Length, not merely leave it stale.
func TestContentLengthTracksShorterBody(t *testing.T) {
	r, err := New(keyword, "x")
	if err != nil {
		t.Fatal(err)
	}

	res, _ := newResponse([]byte(keyword+keyword), "text/plain")
	if err := r.Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := readBody(t, res); string(got) != "xx" || res.Header.Get("Content-Length") != "2" {
		t.Errorf("body = %q, Content-Length = %q", got, res.Header.Get("Content-Length"))
	}
}

// A body with no match must arrive byte-identical, with its validators intact.
func TestUntouchedBodyKeepsHeaders(t *testing.T) {
	res, _ := newResponse([]byte("nothing to see"), "text/plain", withHeader("Etag", `"abc123"`))

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := string(readBody(t, res)); got != "nothing to see" {
		t.Errorf("body = %q", got)
	}
	if res.Header.Get("Etag") != `"abc123"` {
		t.Error("ETag was dropped from a body that did not change")
	}
}

// A response read to EOF must be closed, or the connection is never released.
func TestClosesOriginalBody(t *testing.T) {
	res, tracked := newResponse([]byte(keyword), "text/plain")

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !tracked.closed {
		t.Error("original body was not closed")
	}
}

// --- gzip ---

func TestGzipIsDecompressedRewrittenAndRecompressed(t *testing.T) {
	plain := "<h1>" + keyword + "</h1>"
	res, _ := newResponse(gz(t, []byte(plain)), "text/html; charset=utf-8",
		withHeader("Content-Encoding", "gzip"))

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	body := readBody(t, res)

	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", res.Header.Get("Content-Encoding"))
	}
	if bytes.Contains(body, []byte(replacement)) {
		t.Error("body is not compressed; the replacement is visible in the wire bytes")
	}
	if got := string(gunzipAll(t, body)); got != "<h1>"+replacement+"</h1>" {
		t.Errorf("decompressed body = %q", got)
	}
	// The length must describe the compressed bytes, which is what is sent.
	if want := strconv.Itoa(len(body)); res.Header.Get("Content-Length") != want {
		t.Errorf("Content-Length = %q, want %q", res.Header.Get("Content-Length"), want)
	}
}

// A gzip body with no match must keep its original compressed bytes, not be
// re-compressed into different ones.
func TestGzipWithoutMatchIsByteIdentical(t *testing.T) {
	original := gz(t, []byte("nothing here"))
	res, _ := newResponse(original, "text/html", withHeader("Content-Encoding", "gzip"))

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readBody(t, res); !bytes.Equal(got, original) {
		t.Error("gzip body without a match was not returned byte-identical")
	}
}

// Bytes that claim to be gzip but are not must be forwarded, not turned into an
// error: an unmodified response beats a 502.
func TestUndecodableGzipIsForwardedUnchanged(t *testing.T) {
	original := []byte("not-really-gzip " + keyword)
	res, _ := newResponse(original, "text/html", withHeader("Content-Encoding", "gzip"))

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply returned an error, want passthrough: %v", err)
	}
	if got := readBody(t, res); !bytes.Equal(got, original) {
		t.Errorf("body = %q, want the original bytes", got)
	}
}

// A gzip stream that expands past the decompression guard must be forwarded
// rather than buffered. This is the one bound that remains: there is no cap on
// plain bodies, but a few kilobytes of gzip can expand without limit, so an
// unbounded read here would be the vulnerability rather than a policy choice.
func TestGzipBombIsForwardedUnchanged(t *testing.T) {
	r := newRewriter(t)
	// Lowered so the guard can be proven without building a 100 MiB fixture.
	r.maxDecompressed = testLimit

	bomb := gz(t, bytes.Repeat([]byte(keyword+" "), testLimit/4))
	if len(bomb) >= testLimit {
		t.Fatalf("compressed fixture is %d bytes, too large to prove the point", len(bomb))
	}

	res, _ := newResponse(bomb, "text/html", withHeader("Content-Encoding", "gzip"))

	if err := r.Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readBody(t, res); !bytes.Equal(got, bomb) {
		t.Error("decompression was not bounded; body differs from the original")
	}
}

// The guard's default must be the package constant, not whatever a test set.
func TestDecompressionGuardDefault(t *testing.T) {
	if got := newRewriter(t).maxDecompressed; got != maxDecompressedBytes {
		t.Errorf("maxDecompressed = %d, want %d", got, maxDecompressedBytes)
	}
}

// A gzip body under the guard is rewritten however large it is.
func TestLargeGzipBodyIsRewritten(t *testing.T) {
	plain := bytes.Repeat([]byte("a "+keyword+" b\n"), 100_000)
	res, _ := newResponse(gz(t, plain), "text/html", withHeader("Content-Encoding", "gzip"))

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := gunzipAll(t, readBody(t, res))
	if bytes.Contains(got, []byte(keyword)) {
		t.Error("a large gzip body was left unrewritten")
	}
	if n := bytes.Count(got, []byte(replacement)); n != 100_000 {
		t.Errorf("replaced %d occurrences, want 100000", n)
	}
}

// --- what must be left alone ---

func TestSkipsUnrewritableResponses(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		opts        []responseOption
	}{
		{"png", "image/png", nil},
		{"svg is served as an image", "image/svg+xml", nil},
		{"mp4", "video/mp4", nil},
		{"font", "font/woff2", nil},
		{"zip", "application/zip", nil},
		{"octet-stream", "application/octet-stream", nil},
		{"no content type", "", nil},
		{"unparseable content type", "text/", nil},
		{"utf-16", "text/plain; charset=utf-16", nil},
		{"iso-8859-1", "text/plain; charset=iso-8859-1", nil},
		{"brotli", "text/html", []responseOption{withHeader("Content-Encoding", "br")}},
		{"deflate", "text/html", []responseOption{withHeader("Content-Encoding", "deflate")}},
		{"zstd", "text/html", []responseOption{withHeader("Content-Encoding", "zstd")}},
		{"stacked encodings", "text/html", []responseOption{withHeader("Content-Encoding", "gzip, br")}},
		{"no content", "text/html", []responseOption{withStatus(http.StatusNoContent)}},
		{"not modified", "text/html", []responseOption{withStatus(http.StatusNotModified)}},
		{"informational", "text/html", []responseOption{withStatus(http.StatusContinue)}},
		{"partial content", "text/html", []responseOption{withStatus(http.StatusPartialContent)}},
		{"content range", "text/html", []responseOption{withHeader("Content-Range", "bytes 0-9/100")}},
		// Textual, but open-ended: reading it to the end would never return.
		{"server-sent events", "text/event-stream", nil},
		{"server-sent events with charset", "text/event-stream; charset=utf-8", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte("body mentioning " + keyword)
			res, _ := newResponse(original, tc.contentType, tc.opts...)
			before := res.Header.Clone()

			if err := newRewriter(t).Apply(res); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			if got := readBody(t, res); !bytes.Equal(got, original) {
				t.Errorf("body = %q, want it unmodified", got)
			}
			if got, want := res.Header.Get("Content-Length"), before.Get("Content-Length"); got != want {
				t.Errorf("Content-Length = %q, want %q", got, want)
			}
		})
	}
}

// A HEAD response has no body to rewrite, but its Content-Length would describe
// the body a GET returns — which this proxy rewrites to a different length. A
// stale length is worse than none, so it is dropped.
func TestHeadDropsContentLengthWhenBodyWouldBeRewritten(t *testing.T) {
	res, _ := newResponse(nil, "text/html", withMethod(http.MethodHead))
	res.Header.Set("Content-Length", "146")
	res.ContentLength = 146

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := res.Header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it dropped", got)
	}
	if res.ContentLength != -1 {
		t.Errorf("ContentLength = %d, want -1 (unknown)", res.ContentLength)
	}
}

// A HEAD for a body that would not be rewritten keeps its length: it is accurate.
func TestHeadKeepsContentLengthForBinary(t *testing.T) {
	res, _ := newResponse(nil, "image/png", withMethod(http.MethodHead))
	res.Header.Set("Content-Length", "2048")
	res.ContentLength = 2048

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := res.Header.Get("Content-Length"); got != "2048" {
		t.Errorf("Content-Length = %q, want it preserved", got)
	}
}

// An empty body must not be touched even when its type is rewritable.
func TestSkipsEmptyBody(t *testing.T) {
	res, _ := newResponse(nil, "text/html")

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readBody(t, res); len(got) != 0 {
		t.Errorf("body = %q, want empty", got)
	}
}

// --- large bodies ---

// There is no size cap: a body far larger than any page is still rewritten, and
// every occurrence in it is replaced.
func TestLargeBodyIsRewrittenInFull(t *testing.T) {
	const repeats = 200_000
	original := []byte(strings.Repeat("lorem "+keyword+" ipsum\n", repeats))

	res, tracked := newResponse(original, "text/plain")

	if err := newRewriter(t).Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := readBody(t, res)
	if bytes.Contains(got, []byte(keyword)) {
		t.Error("a large body was left unrewritten")
	}
	if n := bytes.Count(got, []byte(replacement)); n != repeats {
		t.Errorf("replaced %d occurrences, want %d", n, repeats)
	}
	if res.Header.Get("Content-Length") != strconv.Itoa(len(got)) {
		t.Errorf("Content-Length = %q, want %d", res.Header.Get("Content-Length"), len(got))
	}
	if !tracked.closed {
		t.Error("original body was not closed")
	}
}

// --- failure that genuinely cannot be recovered ---

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingBody) Close() error             { return nil }

// Once the body has failed mid-read there is nothing left to forward, so the
// error must surface rather than produce a silently truncated response.
func TestUnreadableBodyReturnsError(t *testing.T) {
	res, _ := newResponse(nil, "text/plain")
	res.Body = failingBody{}
	res.ContentLength = -1

	if err := newRewriter(t).Apply(res); err == nil {
		t.Fatal("Apply succeeded on an unreadable body")
	}
}

// --- construction ---

func TestNewRejectsEmptyKeyword(t *testing.T) {
	_, err := New("", replacement)
	if err == nil {
		t.Fatal("New accepted an empty keyword; it would inject the replacement between every byte")
	}
	if !strings.Contains(err.Error(), "keyword must not be empty") {
		t.Errorf("error = %v", err)
	}
}

// An empty replacement deletes the keyword, which is a legitimate rewrite.
func TestEmptyReplacementDeletesKeyword(t *testing.T) {
	r, err := New(keyword, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, _ := newResponse([]byte("a "+keyword+" b"), "text/plain")
	if err := r.Apply(res); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := string(readBody(t, res)); got != "a  b" {
		t.Errorf("body = %q, want %q", got, "a  b")
	}
}
