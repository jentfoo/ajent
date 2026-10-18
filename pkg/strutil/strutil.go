package strutil

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// FirstLine returns s up to the first newline, or s when it has none. Used for
// tool headers and picker labels that want only the opening line of a command.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// trimZero strips a trailing ".0" from a formatted float so "68.2k", not "68.20k".
func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

// Plural renders a count with its unit, appending "s" for every count but one.
func Plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// FormatTokens abbreviates a token count as 950, 12.5k or 1.2M: decimal units,
// one decimal place, trailing .0 dropped.
func FormatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return trimZero(strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64)) + "M"
	case n >= 1_000:
		return trimZero(strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64)) + "k"
	default:
		return strconv.Itoa(n)
	}
}

// Clip returns s truncated to at most n runes total: when s is cut, the
// last rune of the budget becomes an ellipsis. Returns s whole when s fits,
// and "" when n <= 0.
func Clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n { // the common fit path, no allocation
		return s
	}
	if n == 1 {
		return "…"
	}
	for off := range s { // byte offset where the budget's last rune starts
		if n == 1 {
			return s[:off] + "…"
		}
		n--
	}
	return "" // unreachable: s has more than n runes
}

// HumanSize abbreviates a byte count as 259b, 3.5kb or 1.2mb: binary units,
// one decimal place, trailing .0 dropped. Quantities just below 1mb round up
// to mb, so nothing renders as "1024kb".
func HumanSize(n int64) string {
	const (
		kb = 1024.0
		mb = 1024.0 * 1024.0
	)
	f := float64(n)
	switch {
	case f >= mb || f/kb >= kb-0.05: // a half-step below 1mb rounds up to 1024kb
		return trimZero(strconv.FormatFloat(f/mb, 'f', 1, 64)) + "mb"
	case f >= kb:
		return trimZero(strconv.FormatFloat(f/kb, 'f', 1, 64)) + "kb"
	default:
		return strconv.FormatInt(n, 10) + "b"
	}
}

// Elapsed renders a duration rounded to the second as "41s" or "2m0s", and
// "0s" for anything under one second.
func Elapsed(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Second {
		return "0s"
	}
	return d.String()
}

// FirstArgText returns the first non-empty string field of a JSON object in
// document order, or "" when none. Used to name tool calls by their target,
// so document order keeps identical input on one stable label across replays.
func FirstArgText(input json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(input))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	for dec.More() { // keys arrive in document order
		if _, err := dec.Token(); err != nil { // the key: only its position matters
			return ""
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return "" // truncated input has no stable first field
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
