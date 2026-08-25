package tui

import (
	"bufio"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jentfoo/ajent/pkg/strutil"

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
	resizeSettle = 80 * time.Millisecond
	// after a settled burst the redraw waits on the terminal's status reply;
	// a terminal that never answers gets this much grace before we draw anyway
	resizeProbeTimeout = 300 * time.Millisecond
	// after the reply, the draw waits one more quiet grace: a SIGWINCH already
	// in flight (or delivered late to a busy goroutine) invalidates it, so a
	// frame never goes out alongside a resize we have not seen yet
	resizeDrawGrace = 80 * time.Millisecond
	spinnerInterval = 90 * time.Millisecond
	maxInputRatio   = 2 // input may take at most this fraction of the screen
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
	ControlModeCycle    // Shift+Tab; meaning belongs to the front end
	ControlRecallQueued // Alt+↑: recall the newest queued prompt into the editor
)

// Options configures a UI.
type Options struct {
	In        *os.File // defaults to os.Stdin
	Out       *os.File // defaults to os.Stdout
	Mode      Mode     // paint mode, ModeAuto detects multiplexers
	Palette   Palette  // color set, the zero value uses DefaultPalette
	Model     string
	MaxTokens int
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
	out       outputHead
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
	activity   []activityRow // transient keyed rows above the input, insertion order
	queued     []string      // pending prompts awaiting a steer boundary; oldest first
	noticeKey  string        // keyed notice still collapsible in the live block
	noticeText string

	completer     Completer           // nil disables the completion overlay
	completion    *completionOverlay  // active completion state, nil when closed
	completionSeq int                 // generation guard for async path queries
	historySearch func() []SearchItem // nil disables Ctrl+R history search
	search        *searchOverlay      // active search state, nil when closed

	// plain ↑/↓ browse the same recorded-prompt list that Ctrl+R searches (see
	// SetHistorySearch), so arrows recall prompts without opening the overlay.
	prompts   []string // newest-first prompt texts; nil falls back to editor history
	promptIdx int      // -1 at the live buffer, else index of the recalled prompt
	stashP    string   // live draft held aside while browsing recorded prompts

	pastes map[string]string // placeholder => pasted content, expanded at submit

	started   bool
	lastBlank bool

	// A resize burst holds back all drawing: while SIGWINCHs are still arriving
	// the emulator is mid-reflow, and a frame whose size reads disagree with
	// the emulator's grid under-parks the cursor, stranding rows (a duplicated
	// divider) that no later erase can reach. resizing gates repaint; commits
	// buffer in deferred until the burst settles (see resize). Even the settled
	// redraw waits on a status-probe barrier (probeResize): the ioctl reports
	// the new size before the emulator has finished reflowing to it, so a quiet
	// signal stream alone cannot prove the grid is stable.
	resizing bool
	deferred []histLine
	// resizeSeq counts SIGWINCHs; probeSeq is the burst generation the newest
	// probe belongs to. A reply (or timeout) starts the draw grace only when
	// they still match, and the draw itself re-checks them; a newer signal at
	// either point means another reflow is in flight. probesOut counts probes
	// still unanswered; replies carry no identity, so only the last one in
	// flight can release the barrier.
	resizeSeq int
	probeSeq  int
	probesOut int

	// sigGen is bumped the instant a resize signal arrives, before
	// holdForResize contends for the lock, so a draw already composing can see
	// it without waiting. The draw path compares it around the frame write and
	// abandons a frame whose generation moved: landing it would park by a row
	// count taken on the old grid, the classic stranding miss.
	sigGen atomic.Uint64
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
	theme := NewTheme(DetectColorProfile(osEnv, mode != ModePlain), opts.Palette)

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
	if inl, ok := u.render.(*inlineRenderer); ok {
		inl.sigGen = u.sigGen.Load // only inline parks by row count
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
	u.safeGo(u.watchStatus)
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

// SetTheme recolors the live block and everything committed after it. History
// already on screen keeps the colors it was written with.
func (u *UI) SetTheme(pal Palette) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.theme = NewTheme(u.theme.Profile, pal)
	u.render.setTheme(u.theme)
	u.repaint()
}

// ColorProfile reports the color support the theme was built for.
func (u *UI) ColorProfile() ColorProfile {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.theme.Profile
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
	u.cancelRewindLocked()
	u.cancelInteractions()
	if u.editCh != nil {
		ch := u.editCh
		u.editCh = nil // handleKey's nil guard drops further edits instead of sending on a closed channel
		close(ch)      // ends drainEdits; its range loop exits cleanly
	}
	close(u.done)
	if len(u.deferred) > 0 {
		// a burst overlapping the end of a turn must not swallow committed output
		u.render.commit(u.deferred)
		u.deferred = nil
	}
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
	u.out.reset()
	u.textBuf = ""
	u.streaming = false
	u.textStart = false
	u.started = false // the next gap no longer needs a leading blank line
	u.lastBlank = false
	u.deferred = nil // held-back commits belong to the dropped state
	u.thinking = false
	u.tool = ""
	u.busy = false
	u.activity = nil // transient rows are live-block only; a reset drops them
	u.noticeText = ""
	u.render.clearHistory()
}

// Divider commits a solid full-width band marking the start of restored context.
func (u *UI) Divider() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	u.commitHist([]histLine{{divider: true, style: u.theme.Divider}})
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
	Compact   int // where an auto-compaction would fire; 0 when unset
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
	u.status.Compact = ci.Compact
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

// SetQueued replaces the dimmed pending-prompt rows shown above the input,
// oldest first. The driver (steer queue) owns the list; empty clears.
func (u *UI) SetQueued(texts []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(texts) == 0 && len(u.queued) == 0 {
		return
	}
	u.queued = slices.Clone(texts)
	u.repaint()
}

// PrependInput inserts text at the top of the editor buffer, followed by a
// newline when a draft exists. No-op while an interaction owns the input.
func (u *UI) PrependInput(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.act != nil { // never clobber an active interaction's input
		return
	}
	if cur := u.editor.Value(); cur == "" {
		u.editor.SetValue(text)
	} else {
		u.editor.SetValue(text + "\n" + cur) // cursor lands at the end
	}
	u.repaint()
}

// UserEcho commits a submitted message to history.
func (u *UI) UserEcho(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gap()
	// flowReflow, not flowWrap: pre-wrapping it here would freeze the message at
	// the width it was sent at, and the terminal could never reflow it again.
	// Explicit newlines keep their continuation indent; soft wraps are the
	// terminal's, exactly like the rest of committed output.
	u.commit(indentLines(u.theme.User.Wrap(text), u.theme.User.Wrap(userMarker), userContinue), flowReflow)
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
	if len(done) > 0 {
		// A block completed: refresh the live preview to only what remains uncommitted
		// before committing. Otherwise commit() redraws those just-committed rows as a
		// stale ghost below history and prematurely scrolls fresh output out of view.
		u.repaint()
	}
	u.writeMarkdown(done)
	if len(rest) > 0 && len(done) == 0 {
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
	if rest != "" {
		// Drop the streaming preview before committing it, so commit() does not redraw
		// the same rows as a stale ghost below the new history (see Text).
		u.repaint()
	}
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
	lines := renderPreview(u.theme, w, u.textBuf)
	var out []string
	for _, l := range lines {
		if l.structured() {
			// structured lines carry no text; lay them out or the preview
			// shows a blank row until the block commits
			out = append(out, l.rows(w)...)
		} else {
			out = append(out, wrapLine(l.text, w)...)
		}
	}
	return out
}

// Output streams raw tool output, committed a line at a time with no markdown
// parsing, so log and test output keep their exact shape. Only the first few
// lines reach history; past that an activity row counts the rest as it runs.
func (u *UI) Output(delta string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if head := styleLines(u.theme.Dim, u.out.add(delta)); head != "" {
		u.commit(head, flowWrap)
	}
	if u.out.hidden() > 0 {
		u.setActivityLocked(outputKey, outputRow(&u.out))
	}
}

// EndOutput flushes a partial output line and closes the head.
func (u *UI) EndOutput() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.endOutputLocked()
}

// endOutputLocked commits this call's streamed head, its collapse line and clears
// the activity row. Caller holds the lock.
func (u *UI) endOutputLocked() {
	if tail := styleLines(u.theme.Dim, u.out.flush()); tail != "" {
		u.commit(tail, flowWrap)
	}
	u.commitSummary(&u.out)
	u.setActivityLocked(outputKey, "")
	u.out.reset()
}

// outputRow renders the transient row a long-running tool shows past its head.
func outputRow(h *outputHead) string {
	return "bash · " + strconv.Itoa(h.hidden()) + " lines · " + strutil.HumanSize(int64(h.bytes))
}

// commitSummary appends h's collapse line, indented and dim, when anything is hidden.
func (u *UI) commitSummary(h *outputHead) {
	if sum := h.summary(); sum != "" {
		u.commit(styleLines(u.theme.Dim, indentLines(sum, userContinue, userContinue)), flowWrap)
	}
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
// short name rides in the status bar next to the working glyph (bottom-left
// corner); no separate spinner row is drawn above the input. The returned
// function clears it and commits result, which may be empty when output was
// already streamed.
func (u *UI) ToolStart(name, label string) func(result string) {
	u.mu.Lock()
	label = sanitizeRow(label) // feeds the committed header; name is short and trusted
	// a new call owns the output head; it must never share with a prior one in this turn.
	u.out.reset()
	u.gap()
	u.commit(u.theme.Accent.Wrap(toolMarker)+" "+u.theme.Dim.Wrap(label), flowReflow)
	u.tool = name
	u.spinner = 0
	u.syncSpinnerLocked()
	u.repaint()
	u.mu.Unlock()

	return func(result string) {
		u.mu.Lock()
		defer u.mu.Unlock()
		// close this call's stream before showing its result, so two calls in one
		// turn each get their own head and summary (EndOutput alone waits for TurnEnd).
		u.endOutputLocked()
		u.tool = ""
		if strings.TrimSpace(result) != "" {
			// a non-streaming tool's Display gets the identical head-plus-summary treatment.
			var h outputHead // add returns whole lines; flush picks up any trailing partial
			head := styleLines(u.theme.Dim, h.add(result))
			head += styleLines(u.theme.Dim, h.flush())
			if head != "" {
				u.commit(head, flowWrap)
			}
			u.commitSummary(&h)
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

// commitHist expands tabs, sanitizes and hands the lines to the renderer.
// Sanitizing once here covers every committed surface (inline, alt's retained
// lines, plain), keeping SGR so colored tool output reads, dropping the motion
// and screen escapes caller text may carry.
func (u *UI) commitHist(lines []histLine) {
	if len(lines) == 0 {
		return
	}
	u.flushNotice()               // a keyed notice stops being collapsible once anything follows
	lines = splitHistLines(lines) // histLine.text is single-line by construction
	for i, l := range lines {
		lines[i].text = sanitizeRow(strings.ReplaceAll(l.text, "\t", tabSpaces))
	}
	u.lastBlank = lines[len(lines)-1].text == ""
	u.started = true
	if u.resizing {
		// commit erases the live block too, so it waits out the burst with
		// repaints; resize flushes these in order once the size settles
		u.deferred = append(u.deferred, lines...)
		return
	}
	u.render.commit(lines)
}

// gap separates sections with a single blank line.
func (u *UI) gap() {
	if u.started && !u.lastBlank {
		u.commit("\n", flowReflow)
	}
}

// repaint redraws the live block: notice/streaming/search/completion, activity,
// input or interaction, then the status rows.
func (u *UI) repaint() {
	// While a resize burst is in flight no draw is safe: the erase could land
	// mid-reflow and strand rows (see resize). The settled redraw repaints.
	if u.closed || u.mode == ModePlain || u.resizing {
		return
	}
	w, h := u.render.size()
	// The live block is composed one column short of the terminal. A row that
	// fills the last column leaves the cursor in the deferred-wrap state, and
	// emulators disagree on whether that marks the line as continued; that is
	// what decides how a resize reflows it. Composing narrow (rather than
	// truncating at draw time) means nothing is cut off the editor or a dialog.
	if w > 1 {
		w--
	}

	// the status block is composed first so every budget below it is exact
	st := u.status
	frame := spinnerFrames[u.spinner%len(spinnerFrames)]
	if !u.busy && u.tool == "" {
		frame = spinnerFrames[0] // static resting frame when idle; bottom-left of the cell
	}
	st.Spinner = u.spinnerStyleLocked().Wrap(frame)
	st.Tool = u.tool
	statusRows := st.rows(u.theme, w)

	var rows []string
	if u.noticeText != "" {
		rows = append(rows, u.noticeText)
	}
	// In-progress markdown streams above the input so a reply appears live. It
	// is the one part of the block whose height follows the content rather than
	// the screen, so it yields hardest: a reply longer than the terminal is
	// tall would otherwise make the whole block overflow, and drawing it would
	// scroll the previous frame into scrollback where no erase can reach it,
	// leaving a copy behind on every delta. The tail is what a reader watches.
	if sr := u.streamingRows(w); len(sr) > 0 {
		room := h - len(rows) - 1 - len(statusRows) - 1 // divider, status, input
		if room < len(sr) {
			sr = sr[len(sr)-max(room, 0):]
		}
		rows = append(rows, sr...)
	}
	// a narrow rule atop the prompt area sets it apart from committed and streamed
	// output; row accounting stays exact because this is one more real row here.
	if w > 0 {
		rows = append(rows, u.theme.Dim.Wrap(strings.Repeat(ruleChar, w)))
	}
	offset := len(rows)

	// an active history search rides above the editor like completion; the two are
	// mutually exclusive by construction (opening one clears the other).
	if u.search != nil {
		// a history search needs more room than completion so the full prompt reads;
		// the input keeps its own share below.
		searchRows := u.search.rows(u.theme, w, max(4, (h-1)*2/3))
		rows = append(rows, searchRows...)
		offset += len(searchRows)
	}

	// the completion overlay rides above the editor, shifting the caret offset
	if u.completion != nil && u.act == nil {
		compRows := u.completion.rows(u.theme, w, max(3, (h-2)/4))
		rows = append(rows, compRows...)
		offset += len(compRows)
	}

	// activity yields next on a short terminal: it renders into whatever remains
	// after the status rows and one line for the input/interaction minimum.
	budget := maxActivityBudget // text cap plus the "+N more" row when overflowed
	if room := h - len(rows) - 1 - len(statusRows); room < budget {
		budget = max(room, 0)
	}
	actRows := u.activityRows(w, budget)
	rows = append(rows, actRows...)
	offset += len(actRows)

	// queued pending prompts render after activity and yield like it on a short
	// terminal: they take whatever remains after the status rows and one input line.
	qBudget := maxQueuedBudget
	if room := h - len(rows) - 1 - len(statusRows); room < qBudget {
		qBudget = max(room, 0)
	}
	queuedR := u.queuedRows(w, qBudget)
	rows = append(rows, queuedR...)
	offset += len(queuedR)

	// an interaction takes the input's place while it is active, so the editor
	// keeps whatever was typed and shows it again once the prompt resolves
	var curRow, curCol int
	if u.act != nil {
		var iRows []string
		iRows, curRow, curCol = u.interactionRows(w, h-len(rows))
		rows = append(rows, iRows...)
	} else {
		maxRows := max(1, (h-1)/maxInputRatio)
		var inputRows []string
		inputRows, curRow, curCol = u.editor.inputView(u.theme, w, maxRows)
		rows = append(rows, inputRows...)
	}
	rows = append(rows, statusRows...)

	// Last line of defence on the block's height. Every producer above budgets
	// itself, but a floor (search, completion) or an unlucky width can still
	// push the total past the screen, and a block taller than the screen scrolls
	// as it is drawn; that strands the previous frame's rows above it, one
	// copy per redraw, somewhere no erase can reach. Drop from the top, which is
	// the oldest streamed text, and carry the caret with it.
	caret := offset + curRow
	if over := len(rows) - h; over > 0 && h > 0 {
		rows = rows[over:]
		caret = max(caret-over, 0)
	}
	u.render.setLive(rows, caret, curCol)
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

// spinnerStyleLocked colors the glyph by what the turn is waiting on. Caller holds the lock.
func (u *UI) spinnerStyleLocked() Style {
	switch {
	case u.tool != "":
		return u.theme.SpinnerTool
	case u.streaming || u.thinking:
		return u.theme.SpinnerStream
	case u.busy:
		return u.theme.SpinnerWait
	default:
		return u.theme.Spinner
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

// SetHistorySearch installs the Ctrl+R reverse history search source. fn runs off
// the key loop; nil disables the gesture.
func (u *UI) SetHistorySearch(fn func() []SearchItem) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.historySearch = fn
	if fn == nil {
		u.search = nil // clear any open overlay when the source is removed
	}
	u.repaint()
}

// openSearchLocked opens the Ctrl+R overlay and starts its provider off the key
// loop, so a slow scan never blocks further input. Caller holds the lock.
func (u *UI) openSearchLocked() {
	if u.historySearch == nil {
		return
	}
	fn := u.historySearch
	u.completion = nil
	u.cancelRewindLocked()
	u.search = &searchOverlay{pending: true}
	go func() { u.deliverSearch(fn()) }()
}

// acceptSearchLocked fills the editor with the highlighted match (when there is
// one) and closes the overlay. It does not submit; the caller decides whether to
// fall through so a key like ↑/↓ can then browse history from that point.
// Caller holds the lock.
func (u *UI) acceptSearchLocked() {
	if it, ok := u.search.current(); ok {
		u.editor.SetValue(it.Text)
	}
	u.search = nil
}

// ensurePromptNavLocked lazily loads the recorded-prompt list for plain ↑/↓ once,
// so arrows scroll the same set Ctrl+R searches. A nil or empty source leaves
// prompts nil, in which case arrows fall back to editor history navigation.
// Caller holds the lock; the provider is TTL-cached upstream so this is cheap after
// a first load and never scans per keystroke.
func (u *UI) ensurePromptNavLocked() {
	if u.historySearch == nil || u.prompts != nil {
		return
	}
	items := u.historySearch()
	ps := make([]string, 0, len(items))
	for _, it := range items {
		ps = append(ps, it.Text)
	}
	if len(ps) > 0 {
		u.prompts = ps
		u.promptIdx = -1 // at the live buffer; first ↑ recalls the newest prompt
	} else {
		u.prompts = nil // no recorded prompts: keep editor-history fallback active
	}
}

// promptPrev recalls the next older recorded prompt into the field, holding any live
// draft aside so a later Down restores it. It reports false when there is no list or
// already at the oldest entry, letting callers fall back to editor history.
func (u *UI) promptPrev() bool {
	u.ensurePromptNavLocked()
	if u.prompts == nil || u.promptIdx >= len(u.prompts)-1 {
		return false // no list, or already at the oldest recorded prompt
	}
	if !u.browsingPrompts() { // first step away from the live buffer: hold the draft
		u.stashP = u.editor.Value()
	}
	u.promptIdx++
	u.editor.SetValue(u.prompts[u.promptIdx]) // newest-first, so ↑ walks older
	return true
}

// promptNext moves toward the live buffer, restoring the held draft at the end.
func (u *UI) promptNext() bool {
	if u.prompts == nil || !u.browsingPrompts() {
		return false // no list or already back at the live buffer
	}
	u.promptIdx--
	if u.promptIdx < 0 { // reached the live buffer: bring the draft back
		u.editor.SetValue(u.stashP)
		u.stashP = ""
	} else {
		u.editor.SetValue(u.prompts[u.promptIdx])
	}
	return true
}

// browsingPrompts reports whether ↑/↓ are mid-way through the recorded list rather
// than at the live buffer.
func (u *UI) browsingPrompts() bool {
	return u.prompts != nil && u.promptIdx >= 0
}

// resetPromptNavLocked drops the cached prompt list so a later ↑ reloads it, letting
// freshly recorded prompts appear. Caller holds the lock.
func (u *UI) resetPromptNavLocked() {
	if u.prompts == nil && u.stashP == "" {
		return
	}
	u.prompts = nil
	u.promptIdx = -1
	u.stashP = ""
}

// deliverSearch stores the provider's results if the overlay is still open.
func (u *UI) deliverSearch(items []SearchItem) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.search == nil || !u.search.pending {
		return
	}
	u.search.items = items
	u.search.pending = false
	u.search.refilter()
	u.repaint()
}

// editorWidth returns the width the input is laid out at: one column short of the
// terminal, matching repaint so caret movement tracks the visible rows.
func (u *UI) editorWidth() int {
	w, _ := u.render.size()
	if w > 1 {
		w--
	}
	return w
}

// applyKey mutates the editor for one key and reports whether it changed any
// rendered state. Caller holds the lock.
func (u *UI) applyKey(k key) (submit *string, dirty bool, quit bool) {
	// Shift+Tab is out-of-band: it reaches the control channel even while a
	// dialog or overlay owns the keyboard.
	if k.typ == keyBackTab {
		u.emitControl(ControlModeCycle)
		return nil, false, false
	}
	if u.act != nil {
		u.routeKey(k) // an active interaction sees every key before the editor
		return nil, false, false
	}
	// an open history search consumes keys ahead of completion and the editor. Up
	// and Down select: they commit the highlighted prompt into the field, close the
	// overlay, but do not scroll on this same press; subsequent arrows browse.
	if u.search != nil {
		switch k.typ {
		case keyUp, keyDown:
			u.acceptSearchLocked() // select + close only; no navigation here
			return nil, true, false
		default:
			switch act := u.search.key(k); act {
			case searchStay:
				return nil, true, false
			case searchAccept:
				u.acceptSearchLocked() // Enter commits the highlighted prompt
				return nil, true, false
			case searchClose:
				u.search = nil
				return nil, true, false
			case searchPass:
				u.search = nil // close and let the editor handle this key below
			}
		}
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
				u.resetPromptNavLocked() // let a later ↑ pick up the freshly sent prompt
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
	case keyReverseSearch:
		u.openSearchLocked()
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
			u.resetPromptNavLocked() // let a later ↑ pick up the freshly sent prompt
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
	case keyAltUp:
		u.emitControl(ControlRecallQueued) // the driver updates the editor via SetInput/PrependInput
		return nil, false, false           // no repaint here: those methods repaint
	case keyUp:
		// Cursor-first: move up through visual rows, and only a press already on
		// the very first character (or an empty buffer) recalls older history.
		// This holds even while browsing recorded prompts: a recalled multi-line
		// prompt is editable text whose caret must reach its start before Up steps
		// to the next older entry, and Down restores the live draft from there.
		if !u.editor.Up(u.editorWidth()) {
			// already on the first visual row.
			if u.editor.pos > 0 {
				// mid-text: jump to the prompt's beginning; a second Up recalls history
				u.editor.pos = 0
			} else if !u.promptPrev() { // at the very start (or empty): recall older
				u.editor.HistoryPrev() // fall back when no recorded list exists
			}
		}
	case keyDown:
		// Cursor-first: move down through visual rows, and only a press already on
		// the very last character (or an empty buffer) recalls newer history.
		if !u.editor.Down(u.editorWidth()) {
			// already on the last visual row.
			if u.editor.pos < len(u.editor.cells) {
				// mid-text: jump to the prompt's end; a second Down recalls next
				u.editor.pos = len(u.editor.cells)
			} else if !u.promptNext() { // at the very end (or empty): recall newer
				u.editor.HistoryNext()
			}
		}
	case keyHome:
		u.editor.LineStart(u.editorWidth())
	case keyEnd:
		u.editor.LineEnd(u.editorWidth())
	case keyKillToEnd:
		u.editor.KillToLineEnd(u.editorWidth())
	case keyKillLine:
		u.editor.KillLine()
	case keyKillWord:
		u.editor.KillWordBack()
	case keyRedraw:
		// the fallthrough repaint redraws the live block; committed rows are the
		// terminal's and stay exactly as they are
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

// watchSignals redraws once a burst of resize events settles, and retakes the
// terminal after the process is continued. Drawing never happens mid-burst:
// every frame emitted while the emulator is still reflowing risks parking the
// cursor against a grid that no longer exists, stranding a row no erase can
// reach. A long drag freezes the live block until it pauses; cheap, since the
// emulator is busy mangling the screen then anyway.
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
				continue
			}
			u.sigGen.Add(1) // visible to an in-flight draw before it waits on the lock
			u.holdForResize()
			timer.Reset(resizeSettle)
		case <-timer.C:
			// A SIGWINCH queued behind this timer read means the burst is still
			// going; keep debouncing rather than redraw mid-reflow.
			var pending bool
		drain:
			for {
				select {
				case sig := <-ch:
					if sig == syscall.SIGCONT {
						u.resume()
					} else {
						pending = true
					}
				default:
					break drain
				}
			}
			if pending {
				timer.Reset(resizeSettle)
			} else {
				u.probeResize()
			}
		}
	}
}

// holdForResize gates drawing until the burst settles. Only inline can be
// corrupted by an erase landing mid-reflow; alt owns its screen and plain has
// none, so neither should pay the hold.
func (u *UI) holdForResize() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.resizing = u.mode == ModeInline
	u.resizeSeq++
}

// probeResize starts the settled redraw behind a barrier: the status query is
// answered only after the terminal has processed everything preceding it,
// including the reflow of the resize we just settled on, so the redraw cannot
// land on a grid mid-reflow. The reply (or a timeout, for terminals that never
// answer) drives the actual redraw. Non-inline modes self-heal (alt re-paints
// every cell it owns) or draw nothing (plain), so they settle immediately.
func (u *UI) probeResize() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.mode == ModePlain {
		return
	} else if u.mode != ModeInline {
		u.settleResizeLocked()
		return
	}
	u.probeSeq = u.resizeSeq
	u.probesOut++
	gen := u.probeSeq
	u.render.probe()
	u.afterDelay(resizeProbeTimeout, func() { u.probeTimedOut(gen) })
}

// probeTimedOut arms the settled redraw when the terminal never answered,
// writing off every outstanding probe.
func (u *UI) probeTimedOut(gen int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.probesOut == 0 || gen != u.probeSeq {
		return // a newer burst owns the barrier; its own timeout releases it
	}
	u.probesOut = 0
	u.settleProbedLocked(gen)
}

// settleProbedLocked arms the settled redraw once the barrier is satisfied. A
// stale generation means another reflow is in flight, so it is ignored and the
// newer burst's own probe drives the redraw. Caller holds the lock.
func (u *UI) settleProbedLocked(gen int) {
	if u.closed || gen != u.probeSeq || u.probeSeq != u.resizeSeq {
		return
	}
	// one quiet grace before drawing: the reply proves the terminal caught up,
	// but a resize starting right now (its SIGWINCH perhaps still in flight)
	// would reflow onto the frame we are about to write
	u.afterDelay(resizeDrawGrace, func() { u.drawSettled(gen) })
}

// drawSettled runs the settled redraw once the draw grace elapsed without a
// newer SIGWINCH. During continuous fast resizing every grace is invalidated
// before it fires, so no frame is emitted until a genuine pause; frames and
// reflows never share the wire, which is what strands a divider.
func (u *UI) drawSettled(gen int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || gen != u.probeSeq || u.probeSeq != u.resizeSeq {
		return
	}
	u.settleResizeLocked()
}

// probeAnswered routes a status reply from the input stream to the barrier.
// Replies carry no identity, so one answers the oldest outstanding probe: it
// proves the terminal caught up with the bytes before that probe, not with a
// newer burst's reflow.
func (u *UI) probeAnswered() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.probesOut == 0 {
		return // nothing outstanding: a reply we never asked for, or a late one
	}
	u.probesOut--
	if u.probesOut > 0 {
		return // answers a superseded probe; the newest is still in flight
	}
	u.settleProbedLocked(u.probeSeq)
}

// watchStatus consumes terminal status replies until the input closes.
func (u *UI) watchStatus() {
	for range u.reader.status {
		u.probeAnswered()
	}
}

// resize ends a resize burst: the size goes to the renderer, held-back
// commits flush, and the live block redraws once at the settled size.
func (u *UI) resize() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.mode == ModePlain {
		return
	}
	u.settleResizeLocked()
}

// settleResizeLocked redraws once the terminal size has settled: the renderer
// picks up the new size (alt re-lays and repaints its whole viewport; inline
// just re-reads it and redraws the live block), the live block is recomposed
// (which also drops any deferred rows from its preview, the Text/EndText ghost
// invariant), then held-back commits flush above it. Caller holds the lock.
func (u *UI) settleResizeLocked() {
	u.resizing = false
	u.render.resize()
	u.repaint()
	if len(u.deferred) > 0 {
		lines := u.deferred
		u.deferred = nil
		u.render.commit(lines)
	}
	// A resize landing mid-settle delivers its own SIGWINCH, and the settle
	// that follows redraws at the final size.
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
	u.settleResizeLocked()
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
