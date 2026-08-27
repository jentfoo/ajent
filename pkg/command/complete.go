package command

import (
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/tui"
)

// Completer aggregates command, path and shell completion for the editor: `/`
// at a line start, `@` anywhere, and a leading `!` as bash.
type Completer struct {
	commands *Registry
	console  Console
	paths    *refs.Index
}

// NewCompleter returns a completer sourcing commands from r, argument
// candidates from c, and paths from idx (nil disables path completion).
func NewCompleter(r *Registry, c Console, idx *refs.Index) *Completer {
	return &Completer{commands: r, console: c, paths: idx}
}

// completeKind is what the cursor is completing.
type completeKind uint8

const (
	completeNone completeKind = iota
	completeCommand
	completeArg
	completePath
	completeShell
)

// completeCtx is one classified cursor position.
type completeCtx struct {
	kind  completeKind
	cells []string
	pos   int // cursor cell, clamped to the buffer
	start int // where the context begins: the token, past `!`/`!!`, or the `/` owning the arg
}

// classify reports what the cursor at pos is completing. Complete and Style
// share it so their answers cannot disagree.
func (c *Completer) classify(text string, pos int) completeCtx {
	if pos <= 0 {
		return completeCtx{}
	}
	cells := tui.GraphemeCells(text)
	n := len(cells)
	if n == 0 {
		return completeCtx{}
	}
	ctx := completeCtx{cells: cells, pos: min(pos, n)}

	// a `!` line completes as bash, never as a ref
	if cells[0] == "!" {
		ctx.kind, ctx.start = completeShell, shellCmdStart(cells)
		return ctx
	}

	ctx.start = tokenStart(cells, ctx.pos, 0)
	var first string // "" when the cursor sits on a break or the end
	if ctx.start < n {
		first = cells[ctx.start]
	}
	switch {
	case first == "/" && ctx.start == 0:
		ctx.kind = completeCommand
	case first == "@" && c.paths != nil && ctx.start < ctx.pos:
		ctx.kind = completePath
	default:
		// a /command with args before the cursor
		if ls := lineStartCell(cells, max(ctx.start-1, 0)); ls < n && cells[ls] == "/" {
			ctx.kind, ctx.start = completeArg, ls
		}
	}
	return ctx
}

// Complete returns the cell an accepted Text replaces from, plus the candidates
// at the cursor. pos and start are grapheme-cell indexes, not byte offsets.
func (c *Completer) Complete(text string, pos int) (int, []tui.Completion) {
	ctx := c.classify(text, pos)
	switch ctx.kind {
	case completeShell:
		return c.shellComplete(ctx)
	case completeCommand:
		return c.commandComplete(ctx)
	case completeArg:
		return c.commandArgComplete(ctx)
	case completePath:
		return c.pathComplete(ctx)
	}
	return ctx.pos, nil
}

// Style reports how the cursor's context completes: a command or its argument
// is a live menu, while an `@` path and a `!` shell line are Tab-driven and run
// off the key loop.
func (c *Completer) Style(text string, pos int) tui.CompleteStyle {
	switch c.classify(text, pos).kind {
	case completeCommand, completeArg:
		return tui.CompleteStyle{Menu: true}
	case completePath, completeShell:
		return tui.CompleteStyle{Async: true}
	}
	return tui.CompleteStyle{}
}

// commandComplete offers the command names matching the typed token, matched
// case-insensitively as dispatch resolves it.
func (c *Completer) commandComplete(ctx completeCtx) (int, []tui.Completion) {
	name := strings.ToLower(strings.TrimPrefix(ctx.spanText(ctx.start), "/"))
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
	return offer(ctx.start, ctx.pos, out)
}

// commandArgComplete delegates to the matched command's Complete, resolving the
// name as dispatch does. An empty or absent arg lists every candidate.
func (c *Completer) commandArgComplete(ctx completeCtx) (int, []tui.Completion) {
	nameEnd := tokenEnd(ctx.cells, ctx.start+1)
	// Registry.Get is case sensitive on the lowercased name ParseLine produces
	name := strings.ToLower(strings.Join(ctx.cells[ctx.start+1:nameEnd], ""))
	cmd, ok := c.commands.Get(name)
	if !ok || cmd.Complete == nil {
		return ctx.pos, nil
	}
	argStart := nameEnd + 1 // skip the space after the command name
	return offer(argStart, ctx.pos, textCompletions(cmd.Complete(ctx.spanText(argStart))))
}

// pathComplete offers workspace paths after @.
func (c *Completer) pathComplete(ctx completeCtx) (int, []tui.Completion) {
	// start after @ so accepting keeps the leading @
	return offer(ctx.start+1, ctx.pos, c.paths.Candidates(ctx.spanText(ctx.start+1), nil))
}

// spanText returns the buffer from start up to the cursor.
func (ctx completeCtx) spanText(start int) string {
	if start >= ctx.pos {
		return ""
	}
	return strings.Join(ctx.cells[start:ctx.pos], "")
}

// offer returns start with items, or a no-completion result when items is empty.
func offer(start, pos int, items []tui.Completion) (int, []tui.Completion) {
	if len(items) == 0 {
		return pos, nil
	}
	return start, items
}

// textCompletions turns plain candidate strings into completions.
func textCompletions(items []string) []tui.Completion {
	return bulk.SliceTransform(func(s string) tui.Completion {
		return tui.Completion{Text: s, Label: s}
	}, items)
}

// tokenStart returns the first cell of the token ending at pos, never scanning
// back past from.
func tokenStart(cells []string, pos, from int) int {
	for pos > from && !isTokenBreakCell(cells[pos-1]) {
		pos--
	}
	return pos
}

// tokenEnd returns the cell past the token starting at pos.
func tokenEnd(cells []string, pos int) int {
	for pos < len(cells) && !isTokenBreakCell(cells[pos]) {
		pos++
	}
	return pos
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
