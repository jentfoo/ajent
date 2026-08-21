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

// TrimZero strips a trailing ".0" from a formatted float so "68.2k", not "68.20k".
func TrimZero(s string) string { return strings.TrimSuffix(s, ".0") }

func FormatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return TrimZero(strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64)) + "M"
	case n >= 1_000:
		return TrimZero(strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64)) + "k"
	default:
		return strconv.Itoa(n)
	}
}

// Clip returns s truncated to at most n runes, appending "…" when it was cut.
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
		return TrimZero(strconv.FormatFloat(float64(n)/mb, 'f', 1, 64)) + "mb"
	case float64(n) >= kb:
		return TrimZero(strconv.FormatFloat(float64(n)/kb, 'f', 1, 64)) + "kb"
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

// StripANSI removes ANSI escape sequences from s so captured output stays clean.
func StripANSI(s string) string {
	var out []byte
	for i := 0; i < len(s); {
		if s[i] != 0x1b { // ESC starts an escape sequence to skip over
			out = append(out, s[i])
			i++
			continue
		}
		e := endANSI(s, i) // index just past the full escape sequence
		for ; i < e; i++ { // drop the whole sequence including its final byte
		}
	}
	return string(out)
}

// endANSI returns the index just past the escape sequence beginning at esc.
func endANSI(s string, esc int) int {
	i := esc + 1
	if i >= len(s) {
		return len(s)
	}
	switch s[i] {
	case '[': // CSI ... <final byte in @~>
		for i++; i < len(s); i++ {
			if s[i] >= '@' && s[i] <= '~' {
				return i + 1
			}
		}
	case ']': // OSC ... BEL or ST terminator
		for i++; i < len(s); i++ {
			if s[i] == 0x07 || (s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\') {
				return i + 1
			}
		}
	default: // two-byte escape; drop both bytes
		return esc + 2
	}
	return len(s)
}
