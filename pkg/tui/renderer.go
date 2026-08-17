package tui

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	defaultWidth  = 80
	defaultHeight = 24
)

// Mode selects how the session is painted.
type Mode int

const (
	// ModeAuto picks Inline, or Alt when a multiplexer is detected.
	ModeAuto Mode = iota
	// ModeInline writes history straight to the main screen, so the terminal owns
	// wrapping, reflow and scrollback. Forcing it inside a multiplexer is
	// unsupported: tmux and screen do not reflow, so the caret math goes wrong
	// after a width change.
	ModeInline
	// ModeAlt owns an alternate screen and scrolls itself, so nothing depends on
	// what the emulator does on resize.
	ModeAlt
	// ModePlain writes unadorned lines, for pipes and dumb terminals.
	ModePlain
)

// lineFlow is how a committed line survives a width change.
type lineFlow uint8

const (
	flowReflow lineFlow = iota // prose, the terminal may reflow it
	flowWrap                   // carries alignment: alt wraps it keeping the hanging indent, inline emits it whole like prose
)

// histLine is one committed logical line. text never contains a newline —
// commitHist enforces that by construction (splitHistLines).
type histLine struct {
	text  string
	flow  lineFlow
	table *mdTable // non-nil for a markdown table; laid out fresh at each width
	rule  bool     // true: a horizontal rule drawn to fit the width it is laid at
	style Style    // styling re-applied when rendering a rule (or empty)
}

// rows lays the line out at width, for the renderers that lay history out
// themselves: alt's viewport, inline's structured content, plain's fixed width.
func (l histLine) rows(width int) []string {
	switch {
	case l.table != nil:
		return layoutTable(l.table, width)
	case l.rule:
		// a rule is drawn to fit whatever width it is laid at, not the one it
		// was committed with; style is re-applied so re-laying matches commit.
		txt := strings.Repeat(ruleChar, max(width, minRuleWidth))
		if l.style.Open() != "" {
			txt = l.style.Wrap(txt)
		}
		return []string{txt}
	default:
		return wrapLine(l.text, width)
	}
}

// splitHistLines enforces the single-line invariant on histLine.text. Producers
// are expected to split already, but a multi-line line would silently corrupt
// every renderer (each writes one row per line), so the commit funnel splits
// defensively. A no-op when nothing contains a newline, the common case.
func splitHistLines(lines []histLine) []histLine {
	multi := -1
	for i, l := range lines {
		if strings.Contains(l.text, "\n") {
			multi = i
			break
		}
	}
	if multi < 0 {
		return lines
	}
	out := make([]histLine, 0, len(lines)+1)
	out = append(out, lines[:multi]...)
	for _, l := range lines[multi:] {
		if !strings.Contains(l.text, "\n") {
			out = append(out, l)
			continue
		}
		for _, part := range strings.Split(l.text, "\n") {
			out = append(out, histLine{text: part, flow: l.flow})
		}
	}
	return out
}

// renderer paints committed history and the live block of input and status.
type renderer interface {
	start(inFd int) error
	commit(lines []histLine)
	setLive(rows []string, caretRow, caretCol int)
	clearHistory() // drop retained lines where the mode owns scrollback (alt); no-op elsewhere
	resize()
	probe()                // ask the terminal for a status reply, a barrier against mid-reflow draws
	scroll(lines int) bool // false when the mode has no viewport of its own
	suspend(inFd int)      // hand the terminal back to another program
	resume(inFd int) error // retake it
	close(inFd int)
	size() (width, height int)
}

// ResolveMode returns the paint mode to use. Multiplexers do not reflow their
// buffers on resize, so they get the alternate screen; everything else keeps the
// terminal's native scrollback.
func ResolveMode(want Mode, env func(string) string, isTTY bool) Mode {
	if !isTTY || env("TERM") == "" || env("TERM") == "dumb" {
		return ModePlain
	} else if want != ModeAuto {
		return want
	} else if multiplexed(env) {
		return ModeAlt
	}
	return ModeInline
}

// multiplexed reports whether tmux or screen is between us and the terminal.
func multiplexed(env func(string) string) bool {
	if env("TMUX") != "" || env("STY") != "" {
		return true
	}
	t := env("TERM")
	return strings.HasPrefix(t, "screen") || strings.HasPrefix(t, "tmux")
}

// ParseMode maps a flag value to a Mode.
func ParseMode(s string) (Mode, bool) {
	switch strings.ToLower(s) {
	case "", "auto":
		return ModeAuto, true
	case "inline":
		return ModeInline, true
	case "alt", "altscreen":
		return ModeAlt, true
	case "plain":
		return ModePlain, true
	default:
		return ModeAuto, false
	}
}

// termState owns the terminal itself: raw mode, size and teardown. It is shared
// by every renderer.
type termState struct {
	out           io.Writer
	fd            int
	raw           *term.State
	width, height int
	sizeFn        func() (int, int, error) // nil keeps the current size, used by tests
}

func newTermState(out io.Writer, fd int) *termState {
	t := &termState{
		out:    out,
		fd:     fd,
		width:  defaultWidth,
		height: defaultHeight,
		sizeFn: func() (int, int, error) { return term.GetSize(fd) },
	}
	t.refreshSize()
	return t
}

// refreshSize re-reads the terminal size, keeping the last value on failure.
func (t *termState) refreshSize() {
	if t.sizeFn == nil {
		return
	} else if w, h, err := t.sizeFn(); err == nil && w > 0 && h > 0 {
		t.width, t.height = w, h
	}
}

func (t *termState) makeRaw(inFd int) error {
	if inFd < 0 {
		return nil // no terminal to take, used by tests
	}
	st, err := term.MakeRaw(inFd)
	if err != nil {
		return err
	}
	t.raw = st
	t.refreshSize()
	return nil
}

func (t *termState) restore(inFd int) {
	if t.raw != nil {
		_ = term.Restore(inFd, t.raw)
		t.raw = nil
	}
}

func (t *termState) write(seq string) {
	if seq != "" {
		_, _ = io.WriteString(t.out, seq)
	}
}

// newRenderer builds the renderer for mode.
func newRenderer(mode Mode, theme Theme, out io.Writer, outFd int) renderer {
	t := newTermState(out, outFd)
	switch mode {
	case ModeAlt:
		return &altRenderer{t: t, theme: theme}
	case ModePlain:
		return &plainRenderer{out: out}
	default:
		return &inlineRenderer{t: t}
	}
}

// plainRenderer writes bare lines with no cursor control, for pipes and dumb
// terminals. Its theme carries no color, so nothing it writes is styled.
type plainRenderer struct {
	out io.Writer
}

func (p *plainRenderer) clearHistory()   {} // plain prints lines; nothing retained
func (p *plainRenderer) start(int) error { return nil }

func (p *plainRenderer) commit(lines []histLine) {
	var b strings.Builder
	for _, l := range lines {
		if l.table != nil || l.rule {
			// rows() lays intent out as text; plain's ColorNone theme means a
			// rule's style is empty, so no SGR leaks into a pipe
			for _, row := range l.rows(defaultWidth) {
				b.WriteString(row)
				b.WriteString("\n")
			}
		} else {
			b.WriteString(l.text)
			b.WriteString("\n")
		}
	}
	_, _ = io.WriteString(p.out, b.String())
}

func (p *plainRenderer) setLive([]string, int, int) {}
func (p *plainRenderer) resize()                    {}
func (p *plainRenderer) probe()                     {} // plain draws nothing; no barrier needed
func (p *plainRenderer) scroll(int) bool            { return false }
func (p *plainRenderer) suspend(int)                {}
func (p *plainRenderer) resume(int) error           { return nil }
func (p *plainRenderer) close(int)                  {}
func (p *plainRenderer) size() (int, int)           { return defaultWidth, defaultHeight }

// osEnv is the default environment lookup, replaced in tests.
var osEnv = os.Getenv
