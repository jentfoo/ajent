package command

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/tui"
)

// shellNamesTimeout bounds the one-off compgen lookup.
const shellNamesTimeout = 5 * time.Second

// shellNames returns the sorted command names bash offers as a first word:
// builtins, keywords and everything on PATH. It is empty when bash is
// unavailable. Resolved once per process.
var shellNames = sync.OnceValue(func() []string {
	ctx, cancel := context.WithTimeout(context.Background(), shellNamesTimeout)
	defer cancel()
	// `!` runs through `bash -c`, so compgen under the same non-interactive shell
	// reports exactly the names those runs can invoke
	out, err := exec.CommandContext(ctx, "bash", "-c", "compgen -abck").Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	names := bulk.SliceToSet(strings.Split(string(out), "\n"))
	delete(names, "")
	sorted := bulk.MapKeysSlice(names)
	slices.Sort(sorted)
	return sorted
})

// shellComplete offers candidates inside a `!`/`!!` line: command names in a
// command position, else paths listed one directory at a time. cells is the
// whole buffer and pos the cursor cell; the returned start is where an accepted
// Text begins.
func (c *Completer) shellComplete(cells []string, pos int) (int, []tui.Completion) {
	n := len(cells)
	pos = min(pos, n)
	cmdStart := shellCmdStart(cells)
	if pos < cmdStart {
		return pos, nil
	}
	tokStart := pos
	for tokStart > cmdStart && !isTokenBreakCell(cells[tokStart-1]) {
		tokStart--
	}
	token := strings.Join(cells[tokStart:pos], "")

	// a first word without a separator is a command name; with one it is a path,
	// matching how bash completes `./script` or `bin/tool`
	if !strings.Contains(token, "/") && isCmdPosition(cells, cmdStart, tokStart) {
		if token == "" {
			return pos, nil // every command on the system is not a useful list
		}
		return tokStart, commandNameCandidates(token)
	}
	if c.paths == nil {
		return pos, nil
	}
	items := c.paths.ShellCandidates(token)
	if len(items) == 0 {
		return pos, nil
	}
	return tokStart, items
}

// commandNameCandidates returns the known command names starting with prefix.
func commandNameCandidates(prefix string) []tui.Completion {
	matches := bulk.SliceFilter(func(n string) bool { return strings.HasPrefix(n, prefix) }, shellNames())
	out := make([]tui.Completion, 0, len(matches))
	for _, n := range matches {
		out = append(out, tui.Completion{Text: n, Label: n})
	}
	return out
}

// shellCmdStart returns the cell index where a `!` line's shell text begins,
// past the leading `!` or `!!`.
func shellCmdStart(cells []string) int {
	if len(cells) > 1 && cells[1] == "!" {
		return 2
	}
	return 1
}

// isCmdPosition reports whether the token at start begins a command: only
// whitespace or a command separator lies between it and from.
func isCmdPosition(cells []string, from, start int) bool {
	i := start
	for i > from && isTokenBreakCell(cells[i-1]) {
		i--
	}
	if i <= from {
		return true
	}
	return slices.Contains(cmdSeparators, cells[i-1])
}

// cmdSeparators are the trailing cells of a shell operator that starts a new
// command (`|`, `||`, `&&`, `;`, and a subshell or group opener).
var cmdSeparators = []string{"|", "&", ";", "(", "{"}
