// Package history persists the editor's line history across sessions to
// ~/.ajent/history, deduplicated, capped, and with secret-prefixed lines
// excluded on both load and save so a pasted secret never reaches disk.
package history

import (
	"os"
	"strings"

	"github.com/jentfoo/ajent/pkg/config"
)

// MaxLines caps the persisted editor history so the file stays bounded.
const MaxLines = 1000

// Load reads ~/.ajent/history, deduplicated and capped. Lines starting with
// secretPrefix are excluded so a pasted secret never round-trips. The file is
// oldest-first, newest-last, so the cap keeps the most recent MaxLines.
func Load(secretPrefix string) []string {
	path, err := config.UserPath("history")
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := filter(strings.Split(string(data), "\n"), secretPrefix)
	if len(out) > MaxLines {
		out = out[len(out)-MaxLines:]
	}
	return out
}

// Save writes lines to ~/.ajent/history, replacing any existing file. Lines
// starting with secretPrefix are dropped before writing, and the list is
// capped at MaxLines keeping the most recent.
func Save(lines []string, secretPrefix string) {
	path, err := config.UserPath("history")
	if err != nil {
		return
	}
	filtered := filter(lines, secretPrefix)
	if len(filtered) > MaxLines {
		filtered = filtered[len(filtered)-MaxLines:]
	}
	_ = os.WriteFile(path, []byte(strings.Join(filtered, "\n")+"\n"), 0o600)
}

// filter drops blank, secret-prefixed and duplicate lines, keeping the first
// occurrence of each. It does not cap; the caller caps at the right end.
func filter(lines []string, secretPrefix string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if secretPrefix != "" && strings.HasPrefix(line, secretPrefix) {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}
	return out
}
