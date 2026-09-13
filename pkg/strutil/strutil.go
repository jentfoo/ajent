// Package strutil holds tiny string helpers shared across packages.
package strutil

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
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

// Clip returns s truncated to at most n runes, appending an ellipsis when cut.
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// HumanSize abbreviates a byte count as 259b, 3.5kb or 1.2mb: binary units,
// one decimal place, trailing .0 dropped.
func HumanSize(n int64) string {
	const (
		kb = 1024.0
		mb = 1024.0 * 1024.0
	)
	switch {
	case float64(n) >= mb:
		return trimZero(strconv.FormatFloat(float64(n)/mb, 'f', 1, 64)) + "mb"
	case float64(n) >= kb:
		return trimZero(strconv.FormatFloat(float64(n)/kb, 'f', 1, 64)) + "kb"
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

// FirstArgText returns the first non-empty string field of a JSON object, or ""
// when none. Used to name tool calls by their primary target.
func FirstArgText(input json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	for _, v := range m {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
