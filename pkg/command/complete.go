package command

import (
	"strings"

	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/tui"
)

// Completer aggregates command and path completion for the editor overlay.
// Commands complete at the start of an empty line; paths complete after @
// anywhere. A nil path index means path completion is disabled (plain mode).
type Completer struct {
	commands *Registry
	console  Console
	paths    *refs.Index
}

// NewCompleter returns a completer sourcing commands from r and paths from idx
// (nil disables path completion). c supplies command argument completion.
func NewCompleter(r *Registry, c Console, idx *refs.Index) *Completer {
	return &Completer{commands: r, console: c, paths: idx}
}

// Complete returns candidates at the cursor. / at line start offers commands;
// past the first space it delegates to the matched command's Complete. @
// anywhere offers paths. text is a byte string but pos and the returned start
// are grapheme-cell indexes, matching how the editor addresses its buffer.
func (c *Completer) Complete(text string, pos int) (int, []tui.Completion) {
	if pos <= 0 {
		return 0, nil
	}
	cells := tui.GraphemeCells(text)
	n := len(cells)
	if n == 0 || pos > n {
		pos = min(pos, n)
	}

	// the first cell of the token under the cursor ("" when none: a break or end)
	tokStart := pos
	for tokStart > 0 && !isTokenBreakCell(cells[tokStart-1]) {
		tokStart--
	}
	var first string
	if tokStart < n {
		first = cells[tokStart]
	}

	switch {
	case first == "/" && tokStart == 0:
		return c.commandComplete(cells, pos, tokStart)
	case first == "@":
		return c.pathComplete(cells, pos, tokStart)
	default:
		// a /command with args before the cursor: delegate to its Complete
		if ls := lineStartCell(cells, max(tokStart-1, 0)); ls < n && cells[ls] == "/" {
			return c.commandArgComplete(cells, pos, ls)
		}
	}
	return pos, nil
}

// commandComplete offers command names while the cursor is in the first token,
// or delegates to the matched command's Complete past the first space.
func (c *Completer) commandComplete(cells []string, pos, start int) (int, []tui.Completion) {
	token := strings.Join(cells[start:pos], "")
	rest := strings.TrimPrefix(token, "/")
	name, arg, _ := SplitCommand(rest)
	if arg != "" || strings.Contains(rest, " ") {
		return c.commandArgComplete(cells, pos, start)
	}
	var out []tui.Completion
	for _, cmd := range c.commands.List() {
		if strings.HasPrefix(cmd.Name, name) {
			out = append(out, tui.Completion{
				Text:   "/" + cmd.Name,
				Label:  "/" + cmd.Name,
				Detail: cmd.Description,
			})
		}
	}
	return start, out
}

// commandArgComplete delegates to the matched command's Complete for the arg
// span from ls (the leading /) to the cursor. An empty or absent arg lists all
// candidates.
func (c *Completer) commandArgComplete(cells []string, pos, ls int) (int, []tui.Completion) {
	nameEnd := ls + 1
	for nameEnd < len(cells) && !isTokenBreakCell(cells[nameEnd]) {
		nameEnd++
	}
	name := strings.Join(cells[ls+1:nameEnd], "")
	cmd, ok := c.commands.Get(name)
	if !ok || cmd.Complete == nil {
		return pos, nil
	}
	argStart := nameEnd + 1 // skip the space after the command name
	items := cmd.Complete(strings.Join(cells[argStart:pos], ""))
	if len(items) == 0 {
		return pos, nil
	}
	out := make([]tui.Completion, len(items))
	for i, s := range items {
		out[i] = tui.Completion{Text: s, Label: s}
	}
	return argStart, out
}

// IsAsyncPath reports whether the token under pos is an @ path query. The TUI
// runs such queries off the key loop so a slow directory listing never blocks
// typing; command and argument completion stay synchronous.
func (c *Completer) IsAsyncPath(text string, pos int) bool {
	if c.paths == nil || pos <= 0 {
		return false
	}
	cells := tui.GraphemeCells(text)
	n := len(cells)
	p := min(pos, n)
	tokStart := p
	for tokStart > 0 && !isTokenBreakCell(cells[tokStart-1]) {
		tokStart--
	}
	return tokStart < n && cells[tokStart] == "@"
}

// pathComplete offers workspace paths after @.
func (c *Completer) pathComplete(cells []string, pos, start int) (int, []tui.Completion) {
	if c.paths == nil || start >= len(cells) {
		return pos, nil
	}
	// cursor on the @ with nothing typed after it yet: nothing to complete
	end := min(pos, len(cells))
	if end <= start {
		return pos, nil
	}
	query := strings.Join(cells[start+1:end], "")
	items := c.paths.Candidates(query, nil)
	if len(items) == 0 {
		return pos, nil
	}
	// start after @ so accepting keeps the leading @
	return start + 1, items
}

// isTokenBreakCell reports whether cell ends a completion token.
func isTokenBreakCell(cell string) bool { return cell == " " || cell == "\t" || cell == "\n" }

// lineStartCell returns the index of the first cell on pos's line.
func lineStartCell(cells []string, pos int) int {
	for pos > 0 && cells[pos-1] != "\n" {
		pos--
	}
	return pos
}
