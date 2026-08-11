package tui

import (
	"bufio"
	"io"
	"os"
	"os/signal"
	"slices"
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

// clockwise frames; ⠦ (bottom-left) leads so idle rests at bottom-left
var spinnerFrames = []string{"⠦", "⠧", "⠇", "⠏", "⠋", "⠙", "⠹", "⠸", "⠼", "⠴"}

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
	// History seeds the editor's line history (e.g. loaded from ~/.ajent/history).
	History []string
	// double-Esc rewind: two idle presses within DoubleEscWindow call OnRewind instead of ControlEscape
	DoubleEscWindow time.Duration // window between two idle Esc presses; 0 = default
	OnRewind        func()
	// OnEdit is called with the current editor text (pastes expanded) whenever it
	// changes, so a host can feed token accounting while the user composes. It runs
	// on an internal goroutine, never under the UI lock.
	OnEdit func(text string)
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
	streaming bool // a text block is partially buffered; show it live above input
	textStart bool

	tool      string
	busy      bool // a turn is in flight; the status-bar glyph animates while set
	spinner   int
	spinnerCh chan struct{}

	// input-change notification for token accounting while composing
	onEdit       func(string) // never invoked under u.mu; see editNotify
	editCh       chan string  // coalescing buffer (size 1, latest wins)
	lastNotified string       // last text handed to onEdit, so cursor moves do not refire

	// rewind gesture configuration and state (see rewind.go)
	doubleEscWindow time.Duration
	onRewind        func()
	idle            bool // host marks true while awaiting prompt, false during a turn
	escPending      bool // first idle Esc seen; waiting for a second within the window
	rewTimer        escToken
	afterDelay      func(time.Duration, func()) *time.Timer // time.AfterFunc unless overridden in tests

	act        *pending // interaction owning the live block
	queue      []*pending
	noticeKey  string // keyed notice still collapsible in the live block
	noticeText string

	completer  Completer          // nil disables the overlay
	completion *completionOverlay // active overlay state, nil when closed

	pastes map[string]string // placeholder → pasted content, expanded at submit

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
	if opts.OnEdit != nil {
		u.onEdit = opts.OnEdit
	}
	doubleEsc := opts.DoubleEscWindow
	if doubleEsc <= 0 {
		doubleEsc = defaultDoubleEscWindow
	}
	u.doubleEscWindow = doubleEsc
	u.onRewind = opts.OnRewind
	u.afterDelay = time.AfterFunc
	u.editor.history = slices.Clone(opts.History)
	u.editor.histIdx = len(u.editor.history)

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
	if opts.OnEdit != nil {
		u.editCh = make(chan string, 1)
		u.safeGo(u.drainEdits) // runs OnEdit outside the UI lock
	}
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

// History returns the editor's line history for persistence across sessions.
func (u *UI) History() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.editor.history)
}

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
	u.cancelRewindLocked()
	u.cancelInteractions()
	if u.editCh != nil {
		ch := u.editCh
		u.editCh = nil // handleKey's nil guard drops further edits instead of sending on a closed channel
		close(ch)      // ends drainEdits; its range loop exits cleanly
	}
	close(u.done)
	u.render.close(u.inFd)
}

// Reset drops rendered state so a rewind can redraw just the current session.
// Where the renderer owns scrollback (alt mode) committed lines go too; inline
// keeps the terminal's own scrollback but our buffers and live block reset. The
// next Replay paints the restored branch fresh.
func (u *UI) Reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	u.stopSpinner()
	u.cancelRewindLocked()
	u.thinkBuf.Flush()
	u.outBuf.Flush()
	u.textBuf = ""
	u.streaming = false
	u.textStart = false
	u.started = false // the next gap no longer needs a leading blank line
	u.lastBlank = false
	u.thinking = false
	u.tool = ""
	u.busy = false
	u.noticeText = ""
	u.render.clearHistory()
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

// ContextInfo is how full the next request will be, exact after a response and
// estimated while one streams. It keeps pkg/tui free of any agent import.
type ContextInfo struct {
	Used      int
	Window    int
	Reserve   int
	Estimated bool
}

// SetContext updates the context bar from an accounting snapshot, replacing the
// usage count and reserve while keeping the model's window authoritative. A zero
// Window leaves it untouched so a mid-session /model keeps its own number.
func (u *UI) SetContext(ci ContextInfo) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if ci.Window > 0 {
		u.status.MaxTokens = ci.Window
	}
	u.status.Tokens = ci.Used
	u.status.Reserve = ci.Reserve
	u.status.Estimated = ci.Estimated
	u.repaint()
}

// SetTokens updates the context usage count, leaving the model and its window
// alone. The demo uses it; live sessions drive the bar through SetContext.
func (u *UI) SetTokens(tokens int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.Tokens = tokens
	u.repaint()
}

// SetInput replaces the editor buffer, e.g. to pre-fill a prompt after a rewind.
func (u *UI) SetInput(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.act != nil { // never clobber an active interaction's input
		return
	}
	u.editor.SetValue(text)
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

// Text streams assistant output. Complete markdown blocks commit to history;
// the partial remainder renders live above the input so a reply appears word by
// word instead of only at block boundaries.
func (u *UI) Text(delta string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.textBuf += delta
	u.streaming = true
	done, rest := splitCompleteBlocks(u.textBuf)
	u.textBuf = rest
	u.writeMarkdown(done)
	if len(rest) > 0 {
		u.repaint() // refresh the live preview as the partial block grows
	}
}

// EndText renders whatever remains of the current message and drops the preview.
func (u *UI) EndText() {
	u.mu.Lock()
	defer u.mu.Unlock()
	rest := u.textBuf
	u.textBuf = ""
	u.streaming = false
	u.writeMarkdown(rest)
	u.textStart = false
	u.repaint() // drop the live preview now that the message is committed
}

// Print renders a complete markdown document into history at once, the form
// /help uses. It is the synchronous equivalent of Text followed by EndText.
func (u *UI) Print(markdown string) {
	u.Text(markdown)
	u.EndText()
}

// streamingRows lays the in-progress markdown out into display rows for the live
// block, re-wrapping prose at width so it reads like the finished message.
func (u *UI) streamingRows(w int) []string {
	if !u.streaming || strings.TrimSpace(u.textBuf) == "" {
		return nil
	}
	lines := renderMarkdown(u.theme, w, u.textBuf)
	var out []string
	for _, l := range lines {
		out = append(out, wrapLine(l.text, w)...)
	}
	return out
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

// ToolStart commits the tool header to history. While active, the running tool's
// label rides in the status bar next to the working glyph (bottom-left corner);
// no separate spinner row is drawn above the input. The returned function clears
// it and commits result, which may be empty when output was already streamed.
func (u *UI) ToolStart(label string) func(result string) {
	u.mu.Lock()
	u.gap()
	u.commit(u.theme.Accent.Wrap(toolMarker)+" "+u.theme.Dim.Wrap(label), flowReflow)
	u.tool = label
	u.spinner = 0
	u.syncSpinnerLocked()
	u.repaint()
	u.mu.Unlock()

	return func(result string) {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.tool = ""
		if result != "" {
			u.commit(styleLines(u.theme.Dim, indentLines(result, userContinue, userContinue)), flowWrap)
		}
		// the tool label leaves the status bar; a busy turn keeps its glyph animated
		u.syncSpinnerLocked()
		u.repaint()
	}
}

// Busy makes the status-bar glyph animate for as long as a turn is in flight, so
// there is always an indicator while the model thinks or works. The returned
// function returns it to its static resting frame.
func (u *UI) Busy() func() {
	u.mu.Lock()
	u.busy = true
	u.spinner = 0
	u.syncSpinnerLocked()
	u.repaint()
	u.mu.Unlock()

	return func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		if !u.busy {
			return
		}
		u.busy = false
		u.syncSpinnerLocked()
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
	if u.noticeText != "" {
		rows = append(rows, u.noticeText)
	}
	// in-progress markdown streams above the input so a reply appears live
	rows = append(rows, u.streamingRows(w)...)
	offset := len(rows)

	// the completion overlay rides above the editor, shifting the caret offset
	if u.completion != nil && u.act == nil {
		compRows := u.completion.rows(u.theme, w, max(3, (h-2)/4))
		rows = append(rows, compRows...)
		offset += len(compRows)
	}

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
	// working glyph and running tool live at the status bar's bottom-left
	st := u.status
	frame := spinnerFrames[u.spinner%len(spinnerFrames)]
	if !u.busy && u.tool == "" {
		frame = spinnerFrames[0] // static resting frame when idle; bottom-left of the cell
	}
	st.Spinner = u.theme.Spinner.Wrap(frame)
	st.Tool = u.tool
	rows = append(rows, st.render(u.theme, w))

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

// syncSpinnerLocked keeps the frame ticker running exactly while some indicator
// needs it. Caller holds the lock.
func (u *UI) syncSpinnerLocked() {
	if u.busy || u.tool != "" {
		u.startSpinner()
	} else {
		u.stopSpinner()
	}
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
	if u.editCh != nil && !quit {
		u.notifyEditLocked(u.expandPastes(u.editor.Value()))
	}
	if dirty && !quit {
		u.repaint()
	}
	return submit, quit
}

// SetOnEdit installs an input-change callback after construction, for hosts that
// build their accounting state later than Options (e.g. main's driver). It may be
// called again to swap the callback; drainEdits reads it per iteration so the new
// one takes effect immediately.
func (u *UI) SetOnEdit(fn func(string)) {
	if fn == nil || u.closed {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.editCh != nil && u.onEdit != nil {
		u.onEdit = fn // swap: keep the existing drain goroutine
		return
	}
	u.editCh = make(chan string, 1)
	u.onEdit = fn
	go u.drainEdits()
}

// notifyEditLocked hands the current editor text to OnEdit when it changed since
// the last notification. Caller holds the lock and must have confirmed editCh is
// non-nil; Close clears it before closing so a send here never hits a closed channel.
func (u *UI) notifyEditLocked(text string) {
	if u.onEdit == nil || text == u.lastNotified {
		return
	}
	u.lastNotified = text
	select {
	case u.editCh <- text:
	default: // a newer edit is already queued; drop this one (latest wins)
	}
}

// drainEdits delivers pending editor changes to OnEdit off the UI lock, so the
// callback may safely call back into SetContext or other locked methods. It reads
// onEdit under the lock each iteration so a later SetOnEdit swap takes effect.
func (u *UI) drainEdits() {
	for text := range u.editCh {
		u.mu.Lock()
		fn := u.onEdit
		u.mu.Unlock()
		if fn != nil {
			fn(text)
		}
	}
}

// applyKey mutates the editor for one key and reports whether it changed any
// rendered state. Caller holds the lock.
func (u *UI) applyKey(k key) (submit *string, dirty bool, quit bool) {
	if u.act != nil {
		u.routeKey(k) // an active interaction sees every key before the editor
		return nil, false, false
	}
	// the completion overlay consumes only Tab/↑/↓/Enter/Esc before the editor,
	// and only per the accept rules; everything else falls through so typing
	// still narrows the list.
	if u.completion != nil && u.completion.accept(k) {
		consume, doSubmit := u.completion.key(k, u)
		if consume && k.typ == keyEscape {
			u.completion = nil
			u.cancelRewindLocked()
			return nil, true, false
		}
		if consume && doSubmit {
			// Enter on an unmoved list submits the line as typed
			u.completion = nil
			if v := u.editor.Submit(); strings.TrimSpace(v) != "" {
				e := u.expandPastes(v)
				return &e, true, false
			}
			return nil, true, false
		}
		if consume {
			u.queryCompleter() // re-query after the accept changed the text
			return nil, true, false
		}
	}
	switch k.typ {
	case keyRune:
		u.cancelRewindLocked() // typing ends the idle double-Esc window
		u.editor.Insert(k.text)
	case keyPaste:
		u.cancelRewindLocked()
		text := strings.ReplaceAll(k.text, "\r", "\n")
		if len(text) > pasteThreshold {
			// large paste: store the content and insert a placeholder so the input
			// block stays small; the placeholder is expanded before sending.
			placeholder := pastePlaceholder(text)
			if u.pastes == nil {
				u.pastes = make(map[string]string)
			}
			u.pastes[placeholder] = text
			u.editor.Insert(placeholder)
		} else {
			u.editor.Insert(text)
		}
	case keyNewline:
		u.cancelRewindLocked()
		u.editor.Insert("\n")
	case keyEnter:
		// an unmoved list merely offered: submit the line as typed and close it
		if u.completion != nil {
			u.completion = nil
		}
		if v := u.editor.Submit(); strings.TrimSpace(v) != "" {
			e := u.expandPastes(v)
			return &e, true, false
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
			// Esc clears the buffer rather than rewinding; drop any half-armed
			// gesture so its deferred lone-Esc cannot fire after this press.
			u.cancelRewindLocked()
			u.editor.Clear()
		} else if u.idle && u.onRewind != nil && u.act == nil {
			// idle prompt: defer the lone-Esc emission one window so a second Esc
			// can rewind instead of interrupting (which mid-turn still does)
			if u.escPending { // second Esc inside the window -> rewind gesture
				u.cancelRewindLocked()
				u.triggerRewind()
				return nil, false, false
			}
			u.armRewindLocked()
			return nil, false, false // nothing changed on screen yet
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
	// A key that reached the editor (rather than being consumed by ↑/↓) means the
	// user is composing again: drop any prior selection so Enter submits as typed.
	if u.completion != nil {
		u.completion.moved = false
	}
	// re-query the completer after any editor mutation so the overlay narrows or
	// closes as the text changes; a nil completer is a no-op.
	u.queryCompleter()
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
