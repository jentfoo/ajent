package tui

import (
	"bufio"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	thinkingMarker = "✻"
	thinkingLabel  = " thinking"
	toolMarker     = "⏺"
	userMarker     = "❯ "
	userContinue   = "  "
	tabSpaces      = "    " // tabs are expanded so column math stays exact
	// resize events arrive in bursts while dragging, only rebuild once it settles
	resizeSettle    = 80 * time.Millisecond
	spinnerInterval = 90 * time.Millisecond
	maxInputRatio   = 3 // input may take at most this fraction of the screen
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Control is an out-of-band key the caller decides the meaning of. The editor
// does not consume these; the TUI front end maps them onto agent control calls.
type Control uint8

const (
	ControlEscape Control = iota
	ControlInterrupt
	ControlEOF
)

// Options configures a UI.
type Options struct {
	In        *os.File // defaults to os.Stdin
	Out       *os.File // defaults to os.Stdout
	Mode      Mode     // paint mode, ModeAuto detects multiplexers
	Model     string
	MaxTokens int
}

// UI is the terminal front end: committed history above a live block holding the
// input field and status line. All output methods are safe for concurrent use.
type UI struct {
	mu     sync.Mutex
	theme  Theme
	render renderer
	mode   Mode
	editor editor
	status Status

	in       io.Reader
	inFd     int
	reader   *inputReader
	msgs     chan string
	controls chan Control
	done     chan struct{}
	closed   bool

	thinkBuf  lineBuffer
	thinking  bool
	outBuf    lineBuffer
	textBuf   string
	textStart bool

	tool      string
	spinner   int
	spinnerCh chan struct{}

	act        *pending // interaction owning the live block
	queue      []*pending
	noticeKey  string // keyed notice still collapsible in the live block
	noticeText string

	started   bool
	lastBlank bool
}

// New starts the UI, taking over the terminal until Close is called.
func New(opts Options) (*UI, error) {
	in := opts.In
	if in == nil {
		in = os.Stdin
	}
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}

	isTTY := term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
	mode := ResolveMode(opts.Mode, osEnv, isTTY)
	theme := NewTheme(DetectColorProfile(osEnv, mode != ModePlain))

	u := &UI{
		theme:    theme,
		render:   newRenderer(mode, theme, out, int(out.Fd())),
		mode:     mode,
		status:   Status{Model: opts.Model, MaxTokens: opts.MaxTokens},
		in:       in,
		inFd:     int(in.Fd()),
		msgs:     make(chan string),
		controls: make(chan Control, 4),
		done:     make(chan struct{}),
	}
	if err := u.render.start(u.inFd); err != nil {
		return nil, err
	}
	if mode == ModePlain {
		u.safeGo(u.readLines)
		return u, nil
	}

	u.reader = newInputReader(in)
	u.safeGo(u.reader.run)
	u.safeGo(u.readKeys)
	u.safeGo(u.watchSignals)
	u.repaint()
	return u, nil
}

// safeGo runs fn in a goroutine, restoring the terminal before a panic unwinds.
func (u *UI) safeGo(fn func()) {
	go func() {
		defer func() {
			if p := recover(); p != nil {
				u.Close()
				panic(p)
			}
		}()
		fn()
	}()
}

// Mode reports the paint mode in use.
func (u *UI) Mode() Mode { return u.mode }

// Messages returns submitted user input, closed when the UI goes away.
func (u *UI) Messages() <-chan string { return u.msgs }

// Controls returns keys the editor did not consume: Esc and Ctrl+C or Ctrl+D on
// an empty buffer. The send is non-blocking with a drop, so key handling never
// blocks on the consumer.
func (u *UI) Controls() <-chan Control { return u.controls }

// Width returns the current terminal width in columns.
func (u *UI) Width() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	w, _ := u.render.size()
	return w
}

// Close restores the terminal, it is safe to call more than once.
func (u *UI) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	u.closed = true
	u.stopSpinner()
	u.cancelInteractions()
	close(u.done)
	u.render.close(u.inFd)
}

// SetStatus replaces the whole status line, including the model and every
// segment. To update one part use SetModel, SetTokens or SetStatusSegment,
// which is almost always what a caller means.
func (u *UI) SetStatus(s Status) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = s
	u.repaint()
}

// SetTokens updates the context usage count, leaving the model and its window
// alone. The window belongs to the model, the count belongs to the turn.
func (u *UI) SetTokens(tokens int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.Tokens = tokens
	u.repaint()
}

// UserEcho commits a submitted message to history.
func (u *UI) UserEcho(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gap()
	u.commit(indentLines(u.theme.User.Wrap(text), u.theme.User.Wrap(userMarker), userContinue), flowWrap)
}

// Thinking streams reasoning output, shaded so it does not read as a reply.
func (u *UI) Thinking(delta string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.thinking {
		u.thinking = true
		u.gap()
		u.commit(u.theme.Accent.Wrap(thinkingMarker)+u.theme.Thinking.Wrap(thinkingLabel), flowReflow)
	}
	u.commit(styleLines(u.theme.Thinking, u.thinkBuf.Add(delta)), flowReflow)
}

// EndThinking flushes any partial reasoning line.
func (u *UI) EndThinking() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.commit(styleLines(u.theme.Thinking, u.thinkBuf.Flush()), flowReflow)
	u.thinking = false
}

// Text streams assistant output, rendering markdown once each block is complete.
func (u *UI) Text(delta string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.textBuf += delta
	done, rest := splitCompleteBlocks(u.textBuf)
	u.textBuf = rest
	u.writeMarkdown(done)
}

// EndText renders whatever remains of the current message.
func (u *UI) EndText() {
	u.mu.Lock()
	defer u.mu.Unlock()
	rest := u.textBuf
	u.textBuf = ""
	u.writeMarkdown(rest)
	u.textStart = false
}

// Output streams raw tool output, committed a line at a time with no markdown
// parsing, so log and test output keep their exact shape.
func (u *UI) Output(delta string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.commit(styleLines(u.theme.Dim, u.outBuf.Add(delta)), flowWrap)
}

// EndOutput flushes a partial output line.
func (u *UI) EndOutput() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.commit(styleLines(u.theme.Dim, u.outBuf.Flush()), flowWrap)
}

// Diff commits a colorized diff of a file edit.
func (u *UI) Diff(path, before, after string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := RenderDiff(u.theme, path, before, after)
	if out == "" {
		return
	}
	u.gap()
	u.commit(out, flowWrap)
}

// ToolStart commits the tool header and spins a transient line above the input.
// The returned function clears the spinner and commits result, which may be empty
// when the output was already streamed through Output.
func (u *UI) ToolStart(label string) func(result string) {
	u.mu.Lock()
	u.gap()
	u.commit(u.theme.Accent.Wrap(toolMarker)+" "+u.theme.Dim.Wrap(label), flowReflow)
	u.tool = label
	u.spinner = 0
	u.startSpinner()
	u.repaint()
	u.mu.Unlock()

	return func(result string) {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.tool = ""
		u.stopSpinner()
		if result != "" {
			u.commit(styleLines(u.theme.Dim, indentLines(result, userContinue, userContinue)), flowWrap)
		}
		u.repaint()
	}
}

// writeMarkdown renders and commits complete markdown blocks. Caller holds the lock.
func (u *UI) writeMarkdown(src string) {
	if strings.TrimSpace(src) == "" {
		return
	}
	w, _ := u.render.size()
	lines := renderMarkdown(u.theme, w, src)
	if len(lines) == 0 {
		return
	} else if u.textStart {
		u.commit("\n", flowReflow) // blank line between blocks of the same message
	} else {
		u.gap()
		u.textStart = true
	}
	u.commitHist(lines)
}

// commit splits text into logical lines committed with the given flow.
func (u *UI) commit(text string, flow lineFlow) {
	if text == "" {
		return
	}
	split := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	lines := make([]histLine, len(split))
	for i, l := range split {
		lines[i] = histLine{text: l, flow: flow}
	}
	u.commitHist(lines)
}

// commitHist expands tabs and hands the lines to the renderer.
func (u *UI) commitHist(lines []histLine) {
	if len(lines) == 0 {
		return
	}
	u.flushNotice() // a keyed notice stops being collapsible once anything follows
	for i, l := range lines {
		lines[i].text = strings.ReplaceAll(l.text, "\t", tabSpaces)
	}
	u.lastBlank = lines[len(lines)-1].text == ""
	u.started = true
	u.render.commit(lines)
}

// gap separates sections with a single blank line.
func (u *UI) gap() {
	if u.started && !u.lastBlank {
		u.commit("\n", flowReflow)
	}
}

// repaint redraws the live block: optional tool line, input, status.
func (u *UI) repaint() {
	if u.closed || u.mode == ModePlain {
		return
	}
	w, h := u.render.size()

	var rows []string
	if u.tool != "" {
		frame := spinnerFrames[u.spinner%len(spinnerFrames)]
		rows = append(rows, u.theme.Spinner.Wrap(frame)+" "+u.theme.Dim.Wrap(u.tool))
	}
	if u.noticeText != "" {
		rows = append(rows, u.noticeText)
	}
	offset := len(rows)

	// an interaction takes the input's place while it is active, so the editor
	// keeps whatever was typed and shows it again once the prompt resolves
	var curRow, curCol int
	if u.act != nil {
		var iRows []string
		iRows, curRow, curCol = u.interactionRows(w, h-offset)
		rows = append(rows, iRows...)
	} else {
		maxRows := max(1, (h-1)/maxInputRatio)
		var inputRows []string
		inputRows, curRow, curCol = u.editor.inputView(u.theme, w, maxRows)
		rows = append(rows, inputRows...)
	}
	rows = append(rows, u.status.render(u.theme, w))

	u.render.setLive(rows, offset+curRow, curCol)
}

func (u *UI) startSpinner() {
	if u.spinnerCh != nil {
		return
	}
	ch := make(chan struct{})
	u.spinnerCh = ch
	u.safeGo(func() {
		ticker := time.NewTicker(spinnerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ch:
				return
			case <-ticker.C:
				u.tickSpinner()
			}
		}
	})
}

// tickSpinner advances the spinner one frame.
func (u *UI) tickSpinner() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.spinner++
	u.repaint()
}

func (u *UI) stopSpinner() {
	if u.spinnerCh != nil {
		close(u.spinnerCh)
		u.spinnerCh = nil
	}
}

// readKeys applies decoded keys to the editor until the user quits.
func (u *UI) readKeys() {
	defer close(u.msgs)
	for k := range u.reader.keys {
		if k.typ == keySuspend {
			u.suspend()
			continue
		}
		submit, quit := u.handleKey(k)
		if quit {
			return
		} else if submit != nil {
			select {
			case u.msgs <- *submit:
			case <-u.done:
				return
			}
		}
	}
}

// handleKey applies one key and redraws when it changed the view. A control
// emission or a no-op skips repaint: nothing on screen moved.
func (u *UI) handleKey(k key) (submit *string, quit bool) {
	var dirty bool
	u.mu.Lock()
	defer u.mu.Unlock()
	submit, dirty, quit = u.applyKey(k)
	if dirty && !quit {
		u.repaint()
	}
	return submit, quit
}

// applyKey mutates the editor for one key and reports whether it changed any
// rendered state. Caller holds the lock.
func (u *UI) applyKey(k key) (submit *string, dirty bool, quit bool) {
	if u.act != nil {
		u.routeKey(k) // an active interaction sees every key before the editor
		return nil, false, false
	}
	switch k.typ {
	case keyRune:
		u.editor.Insert(k.text)
	case keyPaste:
		u.editor.Insert(strings.ReplaceAll(k.text, "\r", "\n"))
	case keyNewline:
		u.editor.Insert("\n")
	case keyEnter:
		if v := u.editor.Submit(); strings.TrimSpace(v) != "" {
			return &v, true, false // editor cleared by submit, redraw the prompt
		}
	case keyBackspace:
		u.editor.Backspace()
	case keyDelete:
		u.editor.DeleteForward()
	case keyLeft:
		u.editor.Left()
	case keyRight:
		u.editor.Right()
	case keyWordLeft:
		u.editor.WordLeft()
	case keyWordRight:
		u.editor.WordRight()
	case keyUp:
		if !u.editor.Up() {
			u.editor.HistoryPrev()
		}
	case keyDown:
		if !u.editor.Down() {
			u.editor.HistoryNext()
		}
	case keyHome:
		u.editor.LineStart()
	case keyEnd:
		u.editor.LineEnd()
	case keyKillToEnd:
		u.editor.KillToLineEnd()
	case keyKillLine:
		u.editor.KillLine()
	case keyKillWord:
		u.editor.KillWordBack()
	case keyRedraw:
		u.render.resize()
	case keyPageUp:
		u.render.scroll(u.page())
	case keyPageDown:
		u.render.scroll(-u.page())
	case keyEscape:
		if u.editor.Value() != "" {
			u.editor.Clear()
		} else {
			u.emitControl(ControlEscape) // no visual change
			return nil, false, false
		}
	case keyInterrupt:
		if u.editor.Value() == "" {
			u.emitControl(ControlInterrupt)
			return nil, false, false // no visual change
		} else {
			u.editor.Clear()
		}
	case keyEOF:
		if u.editor.Value() == "" {
			u.emitControl(ControlEOF)
			return nil, false, false // no visual change
		} else {
			u.editor.DeleteForward()
		}
	}
	return nil, true, false
}

// emitControl reports an unconsumed key without blocking. Caller holds the lock.
func (u *UI) emitControl(c Control) {
	select {
	case u.controls <- c:
	default: // nobody is listening, drop rather than stall input
	}
}

// readLines feeds plain mode, where the terminal handles line editing. Reads are
// unbounded, so a large paste arrives whole.
func (u *UI) readLines() {
	defer close(u.msgs)
	r := bufio.NewReader(u.in)
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) != "" {
			select {
			case u.msgs <- line:
			case <-u.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// watchSignals rebuilds the layout once a burst of resize events settles, and
// retakes the terminal after the process is continued.
func (u *UI) watchSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH, syscall.SIGCONT)
	defer signal.Stop(ch)

	timer := time.NewTimer(resizeSettle)
	defer timer.Stop()
	if !timer.Stop() {
		<-timer.C
	}
	for {
		select {
		case <-u.done:
			return
		case sig := <-ch:
			if sig == syscall.SIGCONT {
				u.resume()
			} else {
				timer.Reset(resizeSettle)
			}
		case <-timer.C:
			u.resize()
		}
	}
}

// resize hands the new size to the renderer and redraws the live block.
func (u *UI) resize() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.mode == ModePlain {
		return
	}
	u.render.resize()
	u.repaint()
}

// suspend restores the terminal and stops the process, as Ctrl+Z would outside
// raw mode. The SIGCONT handler retakes the terminal on the way back.
func (u *UI) suspend() {
	u.mu.Lock()
	if u.closed || u.mode == ModePlain {
		u.mu.Unlock()
		return
	}
	u.render.suspend(u.inFd)
	u.mu.Unlock() // released before stopping so Close is never blocked

	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTSTP)
}

// resume retakes the terminal after the process is continued.
func (u *UI) resume() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.mode == ModePlain {
		return
	} else if err := u.render.resume(u.inFd); err != nil {
		return
	}
	u.render.resize()
	u.repaint()
}

// page is one screenful of scrolling, leaving a couple of rows of overlap.
func (u *UI) page() int {
	_, h := u.render.size()
	return max(h-2, 1)
}

// styleLines applies s to each line so the style survives blank lines and gaps.
func styleLines(s Style, text string) string {
	if text == "" || s.Open() == "" {
		return text
	}
	trailing := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, ln := range lines {
		if ln != "" {
			lines[i] = s.Wrap(ln)
		}
	}
	out := strings.Join(lines, "\n")
	if trailing {
		out += "\n"
	}
	return out
}
