package httputil

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
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

// credential query params, compared after lowercasing and stripping the
// separators, so api_key, API-KEY and ApiKey all hit
var redactedQueryParams = []string{
	"key", "apikey", "token", "accesstoken", "auth", "secret", "signature",
}

// secretPattern matches common API key shapes a provider may echo into an
// error body
var secretPattern = regexp.MustCompile(
	`sk-(?:ant-)?[A-Za-z0-9_-]{16,}` + // OpenAI and Anthropic
		`|AIza[0-9A-Za-z_-]{35}` + // Google
		`|Bearer [A-Za-z0-9._~+/=-]{16,}`)

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

// redactURL returns u with userinfo blanked and credential query parameters masked.
func redactURL(u *url.URL) string {
	c := *u
	c.User = nil
	q := c.Query()
	var dirty bool
	for k := range q {
		if credentialParam(k) {
			q[k] = []string{redactedQuery}
			dirty = true
		}
	}
	if dirty {
		c.RawQuery = q.Encode()
	}
	return c.String()
}

// credentialParam reports whether a query parameter name carries a credential.
func credentialParam(k string) bool {
	k = strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(k))
	return slices.Contains(redactedQueryParams, k)
}

// redactErrURL rewrites a transport error's URL in place, so the string a
// caller prints matches the redacted LogEvent.
func redactErrURL(err error, u *url.URL) {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = redactURL(u)
	}
}

// errBodyLimit bounds how much of an error body is kept for the debug log.
const errBodyLimit = 2 << 10

// readErrorBody reads a bounded, scrubbed copy of an error response body.
// secrets are the credential values the request carried, replaced literally
// before the pattern pass.
func readErrorBody(resp *http.Response, secrets []string) []byte {
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
	return scrubSecrets(body, secrets)
}

// scrubSecrets replaces the given credential values and anything matching a
// common key shape with the redaction mask.
func scrubSecrets(body []byte, secrets []string) []byte {
	s := string(body)
	for _, v := range secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, redactedMask)
		}
	}
	return []byte(secretPattern.ReplaceAllLiteralString(s, redactedMask))
}

// secretValues returns the credential header values a request carried, plus
// the bare token of a Bearer pair, which providers echo without the prefix.
func secretValues(h http.Header) []string {
	var out []string
	for _, k := range redactedHeaders {
		if v := h.Get(k); v != "" {
			out = append(out, v)
			if tok, ok := strings.CutPrefix(v, "Bearer "); ok && tok != "" {
				out = append(out, tok)
			}
		}
	}
	return out
}
