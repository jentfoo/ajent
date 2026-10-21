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

// redactRawErrURL rewrites a construction failure's URL in place from the raw
// request string, since no parsed URL exists to drive redactErrURL.
func redactRawErrURL(err error) {
	if err == nil {
		return
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = redactRawURL(ue.URL)
	}
}

// redactRawURL applies the redactURL treatment to a URL string that failed to
// parse: userinfo is dropped and credential query values masked in place, so
// nothing verbatim reaches an error or log event.
func redactRawURL(raw string) string {
	s := cutRawUserinfo(raw)
	i := strings.IndexByte(s, '?')
	if i < 0 {
		return s
	}
	head, rest := s[:i+1], s[i+1:]
	var frag string
	if j := strings.IndexByte(rest, '#'); j >= 0 {
		frag, rest = rest[j:], rest[:j]
	}
	return head + redactRawQuery(rest) + frag
}

// cutRawUserinfo removes the userinfo of a URL string: everything between the
// scheme separator and the last '@' before the first '/', '?' or '#'.
func cutRawUserinfo(raw string) string {
	i := strings.Index(raw, "//")
	if i < 0 {
		return raw
	}
	rest := raw[i+2:]
	end := strings.IndexAny(rest, "/?#")
	auth := rest
	if end >= 0 {
		auth = rest[:end]
	}
	at := strings.LastIndexByte(auth, '@')
	if at < 0 {
		return raw
	}
	return raw[:i+2] + auth[at+1:] + rest[len(auth):]
}

// redactRawQuery masks the value of every credential pair in a raw query
// string.
func redactRawQuery(q string) string {
	pairs := strings.Split(q, "&")
	var dirty bool
	for i, pair := range pairs {
		name, val, hasVal := strings.Cut(pair, "=")
		if !hasVal {
			continue // nothing to mask in a valueless pair
		}
		if dec, err := url.QueryUnescape(name); err == nil {
			name = dec // an encoded name still matches after unescaping
		}
		if !credentialName(name) {
			continue
		}
		pairs[i] = pair[:len(pair)-len(val)] + redactedQuery
		dirty = true
	}
	if !dirty {
		return q
	}
	return strings.Join(pairs, "&")
}

// errBodyLimit bounds how much of an error body is kept for the debug log.
const errBodyLimit = 2 << 10

// scrubMargin overreads past the limit, so a credential straddling the cut is
// masked before clipping instead of surviving as a partial prefix
const scrubMargin = 1 << 10

// readErrorBody reads a bounded, scrubbed copy of an error response body.
// secrets are the credential values the request carried, replaced literally
// after the shape pass.
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
// common key shape with the redaction mask. The pattern pass runs first, so no
// literal replacement can split a longer key past recognition.
func scrubSecrets(body []byte, secrets []string) []byte {
	s := secretPattern.ReplaceAllLiteralString(string(body), redactedMask)
	for _, v := range expandSecrets(secrets) {
		s = strings.ReplaceAll(s, v, redactedMask)
	}
	return []byte(s)
}

// minSecretLen bounds how short a literal replacement may be; shorter values
// cannot be told apart from ordinary body text.
const minSecretLen = 6

// expandSecrets normalizes a credential value list for literal replacement:
// empty, short and prose-shaped values dropped, bare tokens split off scheme
// pairs, longest first so a value that prefixes another cannot truncate it
// into a still-visible tail.
func expandSecrets(vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if !literalSafe(v) { // unfit values drop with the empties
			continue
		}
		scheme, tok, ok := strings.Cut(v, " ")
		if ok && slices.Contains(credentialSchemes, strings.ToLower(scheme)) {
			// a pair scrubs by its whole value plus bare token; an unfit token
			// drops while the whole stays, since matching it needs the exact
			// string. An empty token leaves just the scheme word and drops.
			if tok != "" {
				out = append(out, v)
			}
			if literalSafe(tok) {
				out = append(out, tok)
			}
			continue
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// literalSafe reports whether v is fit for global replacement: long enough to
// tell apart from body noise and not a plain lowercase word, so ordinary text
// survives scrubbing instead of being rewritten.
func literalSafe(v string) bool {
	if len(v) < minSecretLen {
		return false
	}
	// a digit, symbol or upper case marks credential shape; all lower case
	// reads as prose
	return strings.ContainsFunc(v, func(r rune) bool { return r < 'a' || r > 'z' })
}

// secretValues returns the credential values a request carried, from headers
// and URL alike, including custom names beyond the seed list and bare scheme
// tokens.
func secretValues(h http.Header, u *url.URL) []string {
	var vals []string // expanded by expandSecrets below
	for k, vs := range h {
		if credentialName(k) {
			vals = append(vals, vs...)
		}
	}
	return expandSecrets(append(vals, urlSecrets(u)...))
}

// urlSecrets returns the credential values a URL carried: name-matched query
// values and the userinfo username and password. Each appears in every
// spelling an echoed URL can render, raw plus decoded for queries and decoded
// plus re-escaped for userinfo.
func urlSecrets(u *url.URL) []string {
	if u == nil {
		return nil
	}
	var vals []string
	for _, pair := range strings.Split(u.RawQuery, "&") {
		k, v, _ := strings.Cut(pair, "=")
		if dec, err := url.QueryUnescape(k); err == nil {
			k = dec // an encoded name still matches after unescaping
		}
		if !credentialName(k) {
			continue
		}
		vals = append(vals, v)
		if dec, err := url.QueryUnescape(v); err == nil && dec != v {
			vals = append(vals, dec)
		}
	}
	if u.User != nil {
		vals = append(vals, userinfoSpellings(u.User.Username())...)
		if pw, ok := u.User.Password(); ok {
			vals = append(vals, userinfoSpellings(pw)...)
		}
	}
	return vals
}

// userinfoSpellings lists a userinfo component's decoded value plus its
// percent-encoded form when the two differ, matching both renderings of an
// echoed URL.
func userinfoSpellings(s string) []string {
	if s == "" {
		return nil
	}
	if esc := url.User(s).String(); esc != s {
		return []string{s, esc}
	}
	return []string{s}
}
