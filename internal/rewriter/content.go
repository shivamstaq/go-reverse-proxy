package rewriter

import (
	"mime"
	"net/http"
	"strings"
)

// ignore reason for HEAD responses
const reasonHEAD = "HEAD response"

// ignore streamed textual responses
var streamingTypes = map[string]bool{
	"text/event-stream": true,
}

// textualTypes are the rewritable media types that neither the text/* prefix nor
// a +json / +xml suffix already covers. It is a supplement to those rules, not
// the whole set: every text/* type matches without an entry here.
var textualTypes = map[string]bool{
	"application/json":         true,
	"application/xml":          true,
	"application/javascript":   true,
	"application/x-javascript": true,
	"application/ecmascript":   true,
}

// determine why a response must be forwarded unmodified
func skipReason(res *http.Response) string {
	switch {
	case res.StatusCode < http.StatusOK,
		res.StatusCode == http.StatusNoContent,
		res.StatusCode == http.StatusNotModified:
		return "status carries no body"
	case res.StatusCode == http.StatusPartialContent, res.Header.Get("Content-Range") != "":
		return "partial content"
	case res.ContentLength == 0:
		return "empty body"
	}

	if enc := encodingOf(res); enc != "" && enc != "identity" && enc != "gzip" {
		return "unsupported content encoding: " + enc
	}

	contentType := res.Header.Get("Content-Type")
	if contentType == "" {
		// ? undeclared body is treated as non-textual. [assumption]
		return "no content type"
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "unparseable content type: " + contentType
	}
	if streamingTypes[mediaType] {
		return "streaming media type: " + mediaType
	}
	if !isTextual(mediaType) {
		return "non-textual content type: " + mediaType
	}
	// A byte-level replacement is only correct in an ASCII-compatible encoding
	if charset := strings.ToLower(params["charset"]); charset != "" && charset != "utf-8" && charset != "us-ascii" {
		return "unsupported charset: " + charset
	}

	// Final check: if request is HEAD -> forward unmodified
	if res.Request != nil && res.Request.Method == http.MethodHead {
		return reasonHEAD
	}

	return ""
}

func isTextual(mediaType string) bool {
	topLevel, _, _ := strings.Cut(mediaType, "/")
	switch topLevel {
	// Images, video, audio and fonts are excluded even when the subtype looks
	// textual (image/svg+xml is XML): they are named as pass-through without
	// qualification, and treating them uniformly keeps the rule predictable.
	case "image", "video", "audio", "font":
		return false
	case "text":
		return true
	}

	return textualTypes[mediaType] ||
		strings.HasSuffix(mediaType, "+json") ||
		strings.HasSuffix(mediaType, "+xml")
}

// encodingOf normalises Content-Encoding for comparison
// nominal set: "gzip" or "gzip, br"
func encodingOf(res *http.Response) string {
	return strings.ToLower(strings.TrimSpace(res.Header.Get("Content-Encoding")))
}
