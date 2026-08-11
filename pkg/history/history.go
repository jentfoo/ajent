// Package history persists the editor's line history across sessions to
// ~/.ajent/history, deduplicated, capped, and with secret-prefixed lines
// excluded on both load and save so a pasted secret never reaches disk.
package history

import (
	"os"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/config"
)

// MaxLines caps the persisted editor history so the file stays bounded.
const MaxLines = 1000

// Load reads ~/.ajent/history, deduplicated to each line's most recent
// occurrence and capped at MaxLines. Lines starting with secretPrefix are
// dropped so a pasted secret never round-trips. The file is oldest-first,
// newest-last.
func Load(secretPrefix string) []string {
	path, err := config.UserPath("history")
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return normalize(strings.Split(string(data), "\n"), secretPrefix)
}

// Save writes lines to ~/.ajent/history, replacing any existing file. Lines
// starting with secretPrefix are dropped before writing; deduplication keeps
// each line's most recent occurrence and the result is capped at MaxLines.
func Save(lines []string, secretPrefix string) {
	path, err := config.UserPath("history")
	if err != nil {
		return
	}
	out := normalize(lines, secretPrefix)
	_ = os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// normalize trims CRs, drops blank and secret-prefixed lines, keeps only the
// most recent occurrence of each line, then caps at MaxLines from the newest.
func normalize(lines []string, secretPrefix string) []string {
	var clean []string
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if l != "" && (secretPrefix == "" || !strings.HasPrefix(l, secretPrefix)) {
			clean = append(clean, l)
		}
	}
	// dedup to the most recent copy: walk backwards keeping first-seen lines,
	// then reverse to restore original order.
	var out []string
	seen := make(map[string]struct{}, len(clean))
	for i := len(clean) - 1; i >= 0; i-- {
		l := clean[i]
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	slices.Reverse(out)
	if len(out) > MaxLines {
		out = out[len(out)-MaxLines:]
	}
	return out
}
