package httputil

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
)

const redactedMask = "[redacted]"

// redactedQuery avoids the brackets, which url encoding would mangle into
// something unreadable in a debug log
const redactedQuery = "redacted"

// credential names matched exactly once lowercased and stripped of separators,
// so ApiKey, api_key and API-KEY all hit. Seed values only: any name ending in
// a credentialSuffix is masked as well, so custom headers from provider config
// cannot slip through by being unknown here.
var redactedNames = bulk.SliceToSet([]string{
	"authorization", "proxyauthorization", "cookie", "setcookie",
	"openaiorganization", // org id, not secret, kept out of logs anyway
	"auth",
})

// credentialSuffix tails a normalized name carrying a credential; the exact
// list cannot know every provider or user configured header, and compounds
// like client_secret, refresh_token and session_key all reduce to these tails
var credentialSuffixes = []string{"key", "token", "secret", "signature"}

// credentialSchemes are value schemes whose bare token a provider may echo
// into an error body without the scheme prefix
var credentialSchemes = []string{"bearer", "basic", "token"}

// nameSeparators folds the separators header and query names use, so one
// matcher serves both surfaces
var nameSeparators = strings.NewReplacer("_", "", "-", "")

// secretPattern matches common API key shapes a provider may echo into an
// error body
var secretPattern = regexp.MustCompile(
	`sk-(?:ant-)?[A-Za-z0-9_-]{16,}` + // OpenAI and Anthropic
		`|AIza[0-9A-Za-z_-]{35}` + // Google
		`|Bearer [A-Za-z0-9._~+/=-]{16,}`)

// redactHeaders returns a copy with credential headers masked, including names
// beyond the seed list and entries stored under non-canonical keys.
func redactHeaders(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		return nil
	}
	for k := range out {
		if credentialName(k) {
			out[k] = []string{redactedMask}
		}
	}
	return out
}

// redactURL returns u with userinfo blanked and credential query parameters masked.
func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.User = nil
	q := c.Query()
	var dirty bool
	for k := range q {
		if credentialName(k) {
			q[k] = []string{redactedQuery}
			dirty = true
		}
	}
	if dirty {
		c.RawQuery = q.Encode()
	}
	return c.String()
}

// credentialName reports whether a header or query parameter name carries a
// credential. Names compare after lowercasing and stripping separators.
func credentialName(name string) bool {
	n := nameSeparators.Replace(strings.ToLower(name))
	if _, ok := redactedNames[n]; ok {
		return true
	}
	return slices.ContainsFunc(credentialSuffixes, func(s string) bool {
		return strings.HasSuffix(n, s)
	})
}

// redactErrURL rewrites a transport error's URL in place, so the string a
// caller prints matches the redacted LogEvent.
func redactErrURL(err error, u *url.URL) {
	if err == nil || u == nil {
		return
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = redactURL(u)
	}
}

// errBodyLimit bounds how much of an error body is kept for the debug log.
const errBodyLimit = 2 << 10

// scrubMargin overreads past the limit, so a credential straddling the cut is
// masked before clipping instead of surviving as a partial prefix
const scrubMargin = 1 << 10

// readErrorBody reads a bounded, scrubbed copy of an error response body.
// secrets are the credential values the request carried, replaced literally
// before the pattern pass.
func readErrorBody(resp *http.Response, secrets []string) []byte {
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit+scrubMargin))
	// clipping waits for scrubbing so a cut-straddling secret cannot survive
	out := scrubSecrets(raw, secrets)
	if len(out) > errBodyLimit {
		out = out[:errBodyLimit]
	}
	return out
}

// scrubSecrets replaces the given credential values and anything matching a
// common key shape with the redaction mask.
func scrubSecrets(body []byte, secrets []string) []byte {
	s := string(body)
	for _, v := range expandSecrets(secrets) {
		s = strings.ReplaceAll(s, v, redactedMask)
	}
	return []byte(secretPattern.ReplaceAllLiteralString(s, redactedMask))
}

// expandSecrets normalizes a credential value list for literal replacement:
// empty values dropped, bare tokens split off scheme pairs, longest first so a
// value that prefixes another cannot truncate it into a still-visible tail.
func expandSecrets(vals []string) []string {
	out := bulk.SliceFilter(func(v string) bool { return v != "" }, vals)
	for _, v := range vals {
		scheme, tok, ok := strings.Cut(v, " ")
		if !ok || tok == "" || !slices.Contains(credentialSchemes, strings.ToLower(scheme)) {
			continue
		}
		out = append(out, tok)
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// secretValues returns the credential values a request's headers carried,
// including custom names beyond the seed list and bare scheme tokens.
func secretValues(h http.Header) []string {
	var vals []string // expanded by expandSecrets below
	for k, vs := range h {
		if credentialName(k) {
			vals = append(vals, vs...)
		}
	}
	return expandSecrets(vals)
}
