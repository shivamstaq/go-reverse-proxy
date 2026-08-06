package rewriter

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
)

// maxDecompressedBytes bounds how far a gzip body may expand
const maxDecompressedBytes = 100 << 20 // 100 MB

// Rewriter replaces every occurrence of a keyword in the bodies it accepts.
type Rewriter struct {
	keyword     []byte
	replacement []byte
	log         *slog.Logger

	// Overridden in tests so the guard can be exercised without building a
	// 100 MiB fixture.
	maxDecompressed int64
}

// New returns a Rewriter.
func New(keyword, replacement string) (*Rewriter, error) {
	if keyword == "" {
		// bytes.ReplaceAll with an empty pattern injects the replacement between
		// every byte of the body, so this can never be allowed to happen.
		return nil, errors.New("rewriter: keyword must not be empty")
	}

	return &Rewriter{
		keyword:         []byte(keyword),
		replacement:     []byte(replacement),
		log:             slog.Default(),
		maxDecompressed: maxDecompressedBytes,
	}, nil
}

// Apply rewrites res in place. Its signature matches Echo's ModifyResponse hook.
func (r *Rewriter) Apply(res *http.Response) error {
	switch reason := skipReason(res); reason {
	case "":
	case reasonHEAD:
		res.Header.Del("Content-Length")
		res.ContentLength = -1
		r.log.Debug("dropped Content-Length from a HEAD whose body would be rewritten", "path", pathOf(res))
		return nil
	default:
		r.log.Debug("forwarding response unmodified", "reason", reason, "path", pathOf(res))
		return nil
	}

	original := res.Body

	// load the full buffer into memory
	buffered, err := io.ReadAll(original)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	// release transport
	original.Close()

	plain := buffered
	compressed := encodingOf(res) == "gzip"
	if compressed {
		if plain, err = r.gunzip(buffered); err != nil {
			r.log.Warn("gzip body did not decode; forwarding unmodified", "error", err, "path", pathOf(res))
			replaceBody(res, buffered)
			return nil
		}
	}

	if !bytes.Contains(plain, r.keyword) {
		// Nothing to do. Restoring the bytes exactly as they arrived keeps
		// Content-Length, ETag and - for a gzip body - the original compression.
		replaceBody(res, buffered)
		return nil
	}

	rewritten := bytes.ReplaceAll(plain, r.keyword, r.replacement)

	if compressed {
		if rewritten, err = gzipEncode(rewritten); err != nil {
			r.log.Warn("could not re-compress body; forwarding unmodified", "error", err, "path", pathOf(res))
			replaceBody(res, buffered)
			return nil
		}
	}

	replaceBody(res, rewritten)
	describeBody(res, len(rewritten))
	return nil
}

// gunzip decompresses b, refusing to expand past maxDecompressed so a small body
// cannot inflate into an unbounded one.
func (r *Rewriter) gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	// One byte past the guard is how we learn it was exceeded.
	plain, err := io.ReadAll(io.LimitReader(zr, r.maxDecompressed+1))
	if err != nil {
		return nil, err
	}
	if int64(len(plain)) > r.maxDecompressed {
		return nil, fmt.Errorf("decompressed body exceeds %d bytes", r.maxDecompressed)
	}
	return plain, nil
}

func gzipEncode(b []byte) ([]byte, error) {
	var out bytes.Buffer

	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// replaceBody swaps in a buffered body. Headers are untouched: callers that
// changed the bytes must also call describeBody.
func replaceBody(res *http.Response, body []byte) {
	res.Body = io.NopCloser(bytes.NewReader(body))
	res.ContentLength = int64(len(body))
}

// describeBody makes the headers describe the body actually being sent.
func describeBody(res *http.Response, size int) {
	res.Header.Set("Content-Length", strconv.Itoa(size))

	// These describe bytes that no longer exist. Left in place, a cache would
	// serve the old body under a validator that now matches new content.
	res.Header.Del("Etag")
	res.Header.Del("Content-Md5")
	res.Header.Del("Digest")
}

func pathOf(res *http.Response) string {
	if res.Request == nil || res.Request.URL == nil {
		return ""
	}
	return res.Request.URL.Path
}
