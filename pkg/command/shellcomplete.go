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
// builtins, keywords and everything on PATH. Empty when bash is unavailable.
var shellNames = sync.OnceValue(func() []string {
	ctx, cancel := context.WithTimeout(context.Background(), shellNamesTimeout)
	defer cancel()
	// the same non-interactive shell `!` runs through, so the names match
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
// command position, else paths.
func (c *Completer) shellComplete(ctx completeCtx) (int, []tui.Completion) {
	if ctx.pos < ctx.start {
		return ctx.pos, nil
	}
	tokStart := tokenStart(ctx.cells, ctx.pos, ctx.start)
	token := ctx.spanText(tokStart)

	// a first word holding `/` is a path, as bash treats `./script`
	if !strings.Contains(token, "/") && isCmdPosition(ctx.cells, ctx.start, tokStart) {
		if token == "" {
			return ctx.pos, nil // every command on the system is not a useful list
		}
		return offer(tokStart, ctx.pos, textCompletions(shellNameMatches(token)))
	}
	if c.paths == nil {
		return ctx.pos, nil
	}
	return offer(tokStart, ctx.pos, c.paths.ShellCandidates(token))
}

// shellNameMatches returns the known command names starting with prefix.
func shellNameMatches(prefix string) []string {
	return bulk.SliceFilter(func(n string) bool { return strings.HasPrefix(n, prefix) }, shellNames())
}

// shellCmdStart returns the cell where a `!` line's shell text begins.
func shellCmdStart(cells []string) int {
	if len(cells) > 1 && cells[1] == "!" {
		return 2
	}
	return 1
}

// isCmdPosition reports whether the token at start begins a command, i.e. only
// whitespace or a separator lies between it and from.
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

// cmdSeparators are the last cells of an operator starting a new command.
var cmdSeparators = []string{"|", "&", ";", "(", "{"}
