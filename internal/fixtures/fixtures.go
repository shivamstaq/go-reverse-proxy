// Package fixtures is the origin server used to exercise the reverse proxy.
// Every route exists to make one proxy requirement fail loudly if the proxy
// gets it wrong. It is test scaffolding, not part of the proxy itself.
package fixtures

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

// Keyword is the token the proxy is expected to find and replace. It appears in
// every textual fixture, and — deliberately — inside the binary one too.
const Keyword = "test-keyword"

// HTMLBody is served by /html, and gzipped by /gzip.
const HTMLBody = `<!doctype html>
<html><body>
<h1>` + Keyword + `</h1>
<p>The word ` + Keyword + ` appears here in the middle.</p>
<footer>` + Keyword + `</footer>
</body></html>`

// BlockSize is the unit in which /large streams its response.
const BlockSize = 1 << 20

// StreamGap is how long /events waits between its two events. A client that
// receives the first one sooner than this was served by a streaming proxy.
const StreamGap = 1500 * time.Millisecond

var largeBlock = func() []byte {
	line := []byte("lorem ipsum " + Keyword + " dolor sit amet\n")
	return bytes.Repeat(line, BlockSize/len(line)+1)[:BlockSize]
}()

// PNGBytes is the exact image /image serves, for byte-identity assertions.
var PNGBytes = pngWithKeyword()

// NewServer builds the fixture origin server.
func NewServer() *echo.Echo {
	e := echo.New()

	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())

	// --- textual bodies: all must be rewritten by the proxy ---

	// Keyword sits at the very start and the very end, so an off-by-one in the
	// proxy's replacement shows up immediately.
	e.GET("/text", func(c *echo.Context) error {
		return c.String(http.StatusOK, Keyword+" at the start.\nA "+Keyword+" in the middle.\nEnds with "+Keyword)
	})

	// HEAD as well as GET: Echo does not derive one from the other, and without
	// it a proxy's HEAD handling cannot be observed here at all.
	html := func(c *echo.Context) error {
		return c.HTML(http.StatusOK, HTMLBody)
	}
	e.GET("/html", html)
	e.Add(http.MethodHead, "/html", html)

	e.GET("/json", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{
			"message": "hello from " + Keyword,
			"vendor":  Keyword,
		})
	})

	e.GET("/xml", func(c *echo.Context) error {
		body := `<?xml version="1.0" encoding="UTF-8"?>` +
			`<root><vendor>` + Keyword + `</vendor><note>about ` + Keyword + `</note></root>`
		return c.Blob(http.StatusOK, "application/xml; charset=utf-8", []byte(body))
	})

	e.GET("/js", func(c *echo.Context) error {
		body := `const vendor = "` + Keyword + `";` + "\n" +
			`console.log("hello from " + vendor);`
		return c.Blob(http.StatusOK, "application/javascript; charset=utf-8", []byte(body))
	})

	// --- content encodings ---

	// Same bytes as /html, gzipped. The proxy must decompress, rewrite, and
	// re-emit with headers that still describe the body it actually sends.
	e.GET("/gzip", func(c *echo.Context) error {
		c.Response().Header().Set("Content-Encoding", "gzip")
		return c.Blob(http.StatusOK, "text/html; charset=utf-8", gzipBytes([]byte(HTMLBody)))
	})

	// Deliberately NOT valid brotli: the proxy cannot decode br, so it must pass
	// these bytes through untouched. If it ever tries to decode, it fails loudly.
	e.GET("/brotli", func(c *echo.Context) error {
		c.Response().Header().Set("Content-Encoding", "br")
		return c.Blob(http.StatusOK, "text/html; charset=utf-8", []byte("not-really-brotli "+Keyword))
	})

	// --- binary: must survive the proxy byte-for-byte ---

	// A valid PNG carrying the keyword in a tEXt chunk. A proxy that rewrites
	// binaries corrupts the image, which a checksum comparison catches.
	e.GET("/image", func(c *echo.Context) error {
		return c.Blob(http.StatusOK, "image/png", PNGBytes)
	})

	// --- transfer behaviour ---

	// Streams ?mb= megabytes (default 50) so buffering strategies show their cost.
	e.GET("/large", func(c *echo.Context) error {
		mb := 50
		if v := c.Request().URL.Query().Get("mb"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return c.String(http.StatusBadRequest, "bad mb")
			}
			mb = n
		}
		w := c.Response()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		for range mb {
			if _, err := w.Write(largeBlock); err != nil {
				return err
			}
		}
		return nil
	})

	// Flushing before the handler returns forces chunked encoding with no
	// Content-Length, which is the case a length-recomputing proxy can trip on.
	e.GET("/chunked", func(c *echo.Context) error {
		w := c.Response()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i, part := range []string{
			"first chunk mentions " + Keyword + "\n",
			"second chunk mentions " + Keyword + "\n",
			"third chunk mentions " + Keyword + "\n",
		} {
			if _, err := io.WriteString(w, part); err != nil {
				return err
			}
			if flusher != nil && i < 2 {
				flusher.Flush()
			}
		}
		return nil
	})

	// Server-sent events: a body that stays open. A proxy that buffers this to
	// rewrite it cannot send anything until the stream ends, so the first event
	// arrives no sooner than StreamGap — which is what makes the difference
	// between streaming and buffering measurable from the client.
	e.GET("/events", func(c *echo.Context) error {
		w := c.Response()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		for i := range 2 {
			if _, err := fmt.Fprintf(w, "data: event %d mentions %s\n\n", i, Keyword); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			if i == 0 {
				select {
				case <-c.Request().Context().Done():
					return nil
				case <-time.After(StreamGap):
				}
			}
		}
		return nil
	})

	// --- status and header preservation ---

	e.GET("/status/:code", func(c *echo.Context) error {
		code, err := strconv.Atoi(c.Param("code"))
		if err != nil || code < 100 || code > 599 {
			return c.String(http.StatusBadRequest, "bad status code")
		}
		// 204 and 304 must not carry a body.
		if code == http.StatusNoContent || code == http.StatusNotModified {
			return c.NoContent(code)
		}
		return c.String(code, Keyword+" body for status "+strconv.Itoa(code))
	})

	e.GET("/redirect", func(c *echo.Context) error {
		return c.Redirect(http.StatusFound, "/html")
	})

	e.GET("/headers", func(c *echo.Context) error {
		h := c.Response().Header()
		h.Set("X-Custom-Header", "custom-"+Keyword)
		h.Add("Set-Cookie", "session=abc123; Path=/; HttpOnly")
		return c.String(http.StatusOK, Keyword+" with headers")
	})

	// --- method and request-body passthrough ---

	e.Any("/echo", func(c *echo.Context) error {
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		// Host is deliberately reported separately: Go lifts it out of the
		// header map into Request.Host, so it is invisible in "headers".
		return c.JSON(http.StatusOK, map[string]any{
			"method":  c.Request().Method,
			"host":    c.Request().Host,
			"headers": c.Request().Header,
			"body":    string(body),
		})
	})

	return e
}
