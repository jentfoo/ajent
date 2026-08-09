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
	flowWrap                   // we wrap, keeping hanging indent
	flowClip                   // structural, clipped not wrapped
)

// histLine is one committed logical line.
type histLine struct {
	text string
	flow lineFlow
}

// rows lays the line out at width, for renderers that own their viewport.
func (l histLine) rows(width int) []string {
	if l.flow == flowClip {
		return []string{l.text} // the caller truncates it to the viewport
	}
	return wrapLine(l.text, width)
}

// renderer paints committed history and the live block of input and status.
type renderer interface {
	start(inFd int) error
	commit(lines []histLine)
	setLive(rows []string, caretRow, caretCol int)
	resize()
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

func (p *plainRenderer) start(int) error { return nil }

func (p *plainRenderer) commit(lines []histLine) {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.text)
		b.WriteString("\n")
	}
	_, _ = io.WriteString(p.out, b.String())
}

func (p *plainRenderer) setLive([]string, int, int) {}
func (p *plainRenderer) resize()                    {}
func (p *plainRenderer) scroll(int) bool            { return false }
func (p *plainRenderer) suspend(int)                {}
func (p *plainRenderer) resume(int) error           { return nil }
func (p *plainRenderer) close(int)                  {}
func (p *plainRenderer) size() (int, int)           { return defaultWidth, defaultHeight }

// osEnv is the default environment lookup, replaced in tests.
var osEnv = os.Getenv
