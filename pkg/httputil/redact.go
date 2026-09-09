package httputil

import (
	"io"
	"net/http"
	"net/url"
)

const redactedMask = "[redacted]"

// redactedQuery avoids the brackets, which url encoding would mangle into
// something unreadable in a debug log
const redactedQuery = "redacted"

// credential headers, matched case insensitively by http.Header canonicalization
var redactedHeaders = []string{
	"Authorization", "X-Api-Key", "Api-Key", "Proxy-Authorization",
	"Cookie", "Set-Cookie", "X-Goog-Api-Key", "Openai-Organization",
}

// redactHeaders returns a copy with credential headers masked.
func redactHeaders(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		return nil
	}
	for _, k := range redactedHeaders {
		if out.Get(k) != "" {
			out.Set(k, redactedMask)
		}
	}
	return out
}

// redactURL returns u with credential query parameters masked.
func redactURL(u *url.URL) string {
	q := u.Query()
	var dirty bool
	for _, k := range []string{"key", "api_key", "access_token"} {
		if q.Has(k) {
			q.Set(k, redactedQuery)
			dirty = true
		}
	}
	if !dirty {
		return u.String()
	}
	c := *u
	c.RawQuery = q.Encode()
	return c.String()
}

// errBodyLimit bounds how much of an error body is kept for the debug log.
const errBodyLimit = 2 << 10

// readErrorBody reads a bounded, scrubbed copy of an error response body.
func readErrorBody(resp *http.Response) []byte {
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
	return body
}
