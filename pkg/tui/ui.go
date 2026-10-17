package tui

import (
	"bufio"
	"errors"
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
	"unicode/utf8"

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
	// marks a streaming preview whose head was dropped to fit the screen
	previewElided = "•••"
	// resize events arrive in bursts while dragging, only rebuild once it settles
	resizeSettle = 80 * time.Millisecond
	// after a settled burst the redraw waits on the terminal's status reply.
	// A terminal that never answers gets this much grace before we draw anyway.
	resizeProbeTimeout = 300 * time.Millisecond
	// after the reply, the draw waits one more quiet grace: a SIGWINCH already
	// in flight (or delivered late to a busy goroutine) invalidates it, so a
	// frame never goes out alongside a resize we have not seen yet
	resizeDrawGrace = 80 * time.Millisecond
	spinnerInterval = 90 * time.Millisecond
	maxInputRatio   = 2 // input may take at most this fraction of the screen
	// cap on how much of a newline-free reasoning tail is laid out per repaint,
	// so one enormous line cannot make every delta O(len) in wrapLine
	thinkingPreviewRunes = 8192
	// cap on how much buffered markdown preview is parsed and wrapped per frame;
	// an unclosed fence or one enormous line would otherwise put O(len) grapheme
	// segmentation under the lock on every tick and delta
	streamPreviewRunes = 8192
)

// clockwise frames, with ⠦ (bottom-left) leading so idle rests at bottom-left
var spinnerFrames = []string{"⠦", "⠧", "⠇", "⠏", "⠋", "⠙", "⠹", "⠸", "⠼", "⠴"}

// Control is an out-of-band key the caller decides the meaning of. The editor
// does not consume these, and the TUI front end maps them onto agent control calls.
type Control uint8

const (
	ControlEscape Control = iota
	ControlInterrupt
	ControlEOF
	ControlModeCycle      // Shift+Tab or Shift+→, meaning belongs to the front end
	ControlRecallQueued   // Alt+↑: recall the newest queued prompt into the editor
	ControlClipboardImage // Ctrl+V: probe the clipboard for an image
	ControlModeCycleBack  // Shift+←: like ControlModeCycle, one mode the other way
	ControlCopySelection  // Ctrl+X: copy the highlighted picker row (rewind tree)
)

// Options configures a UI.
type Options struct {
	In         *os.File     // defaults to os.Stdin
	Out        *os.File     // defaults to os.Stdout
	Mode       Mode         // paint mode, ModeAuto detects multiplexers
	Color      ColorProfile // color depth, ColorAuto detects from TERM/COLORTERM
	Palette    Palette      // color set, the zero value uses DefaultPalette
	Model      string
	ModelShort string // collapse target for Model on a narrow status row
	MaxTokens  int
	// OnEdit is called with the current editor text (pastes expanded) whenever it
	// changes, so a host can feed token accounting while the user composes. It runs
	// on an internal goroutine, never under the UI lock.
	OnEdit func(text string)
	// Images forces the terminal image protocol: "kitty", "iterm2", "none", or
	// "" to detect from the environment. Detection is conservative: multiplexers
	// and unknown terminals report none and every image renders as a placeholder.
	Images string
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

	images ImageProtocol // resolved image protocol, ImageNone hides them
	// kitty image ids to occupied rows, bounded. Deliberately survives Reset:
	// inline scrollback keeps the drawings visible, and these ids are the only
	// handle for freeing their terminal-side data.
	imgIDs map[uint32]int
	imgSeq uint32 // source of fresh kitty image ids
	// terminal cell pixel size from the CSI 16 t reply, consulted by commit
	cellW, cellH int
	closed       bool

	thinkBuf  lineBuffer
	thinking  bool
	textBuf   string
	streaming bool // a text block is partially buffered, show it live above input
	textStart bool

	// preview memos for the per-frame live block. Keys cover every render input
	// except the theme, so SetTheme clears both.
	streamKey  string
	streamW    int
	streamRows []string
	streamOK   bool
	thinkKey   string
	thinkW     int
	thinkRows  []string
	thinkOK    bool

	runs      []*toolRun // in-flight tool calls, oldest first, the newest drives the spinner color
	busy      bool       // a turn is in flight, the status-bar glyph animates while set
	spinner   int
	spinnerCh chan struct{}

	// input-change notification for token accounting while composing
	onEdit       func(string) // never invoked under u.mu, see editNotify
	editCh       chan string  // coalescing buffer (size 1, latest wins)
	lastNotified string       // last text handed to onEdit, so cursor moves do not refire

	// rewind gesture configuration and state (see rewind.go)
	doubleEscWindow time.Duration
	onRewind        func()
	idle            bool // host marks true while awaiting prompt, false during a turn
	escPending      bool // first idle Esc seen, waiting for a second within the window
	rewTimer        escToken
	afterDelay      func(time.Duration, func()) *time.Timer // time.AfterFunc unless overridden in tests

	act        *pending // interaction owning the live block
	queue      []*pending
	activity   []activityRow // transient keyed rows above the input, insertion order
	queued     []string      // pending prompts awaiting a steer boundary, oldest first
	noticeKey  string        // keyed notice still collapsible in the live block
	noticeText string

	completer     Completer           // nil disables the completion overlay
	completion    *completionOverlay  // live menu or an ambiguous Tab's listing, nil when none
	completionSeq int                 // generation guard for async path queries
	ruleFlash     bool                // prompt rule drawn accented until the flash timer clears it
	historySearch func() []SearchItem // nil disables Ctrl+R history search
	promptHistory func() []string     // plain ↑/↓ recall list, nil falls back to historySearch texts
	search        *searchOverlay      // active search state, nil when closed

	// plain ↑/↓ browse the list SetPromptHistory installs, falling back to the
	// history-search source's texts when none is set or it comes back empty
	// (a fresh session has no prompts yet, so arrows still reach prior sessions).
	// Every non-recall buffer replacement calls resetPromptNavLocked, or an
	// arrow at a buffer edge would clobber fresh content with a stale entry.
	prompts     []string // newest-first prompt texts, nil falls back to editor history
	promptIdx   int      // -1 at the live buffer, else index of the recalled prompt
	stashP      string   // live draft held aside while browsing recorded prompts
	promptSeq   uint64   // generation counter, each armed list load adopts the next value
	promptLoad  uint64   // the in-flight load's generation, 0 when idle
	promptEmpty bool     // last load resolved empty: no re-arm until reset

	pastes   []pasteEntry // large pastes in arrival order, expanded at submit
	pasteSeq int          // numbers each placeholder so they stay unique

	started   bool
	lastBlank bool

	// A resize burst holds back all drawing: while SIGWINCHs are still arriving
	// the emulator is mid-reflow, and a frame whose size reads disagree with
	// the emulator's grid under-parks the cursor, stranding rows (a duplicated
	// divider) that no later erase can reach. resizing gates repaint, and commits
	// buffer in deferred until the burst settles (see resize). Even the settled
	// redraw waits on a status-probe barrier (probeResize): the ioctl reports
	// the new size before the emulator has finished reflowing to it, so a quiet
	// signal stream alone cannot prove the grid is stable. resizing is atomic so
	// the signal goroutine can raise it before contending for u.mu: a streaming
	// Text holds the lock across a goldmark parse, and a gate taken only under
	// the lock let a whole frame go out after the reflow had started.
	resizing atomic.Bool
	deferred []histLine
	// resizeSeq counts SIGWINCHs, and probeSeq is the burst generation the newest
	// probe belongs to. A reply (or timeout) starts the draw grace only when
	// they still match, and the draw itself re-checks them. A newer signal at
	// either point means another reflow is in flight. probesOut counts probes
	// still unanswered, since replies carry no identity: only the last one in
	// flight can release the barrier.
	resizeSeq int
	probeSeq  int
	probesOut int
	// cprPending marks a probe whose cursor reply this settle may act on, so a
	// settle that never probed (SIGCONT, resize) cannot re-anchor on a report
	// left over from an earlier burst.
	cprPending bool
	// settledW and settledH are the size the last settle drew at. A settle that
	// only grew never re-anchors: nothing can be lost by a grow, so the pad
	// would be a visible jump for a repair nobody needs.
	settledW int
	settledH int

	// sigGen is bumped the instant a resize signal arrives, before holdForResize
	// contends for the lock. drawGen is the generation the settled redraw last
	// caught up with, and holdGen is the last generation holdForResize sequenced
	// under the lock. The draw path abandons any frame written while sigGen and
	// drawGen differ: landing it would park by a row count taken on the old
	// grid, the classic stranding miss. A settle only clears the gate while
	// sigGen equals holdGen. Otherwise a signal that bumped but has not
	// reached holdForResize would be absorbed into drawGen and a frame could
	// land mid-reflow with neither gate raised.
	sigGen  atomic.Uint64
	drawGen atomic.Uint64
	holdGen atomic.Uint64
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
	theme := NewTheme(DetectColorProfile(opts.Color, osEnv, mode != ModePlain), opts.Palette)

	u := &UI{
		theme:    theme,
		render:   newRenderer(mode, theme, out, int(out.Fd())),
		mode:     mode,
		editor:   editor{shell: true}, // the prompt row: a leading `!` is its marker
		status:   Status{Model: opts.Model, ModelShort: opts.ModelShort, MaxTokens: opts.MaxTokens},
		in:       in,
		inFd:     int(in.Fd()),
		msgs:     make(chan string),
		controls: make(chan Control, 4),
		done:     make(chan struct{}),
		imgIDs:   make(map[uint32]int, maxTrackedImages),
		images:   DetectImageProtocol(opts.Images, osEnv, isTTY),
	}
	if opts.OnEdit != nil {
		u.onEdit = opts.OnEdit
	}
	u.doubleEscWindow = defaultDoubleEscWindow
	u.afterDelay = time.AfterFunc
	if inl, ok := u.render.(*inlineRenderer); ok {
		// only inline parks by row count
		inl.sigGen, inl.drawGen = u.sigGen.Load, u.drawGen.Load
		u.settledW, u.settledH = u.render.size() // the startup size: a first-settle grow must not pad
	}

	if err := u.render.start(u.inFd); err != nil {
		return nil, err
	}
	if mode == ModePlain {
		u.safeGo(u.readLines)
		return u, nil
	}

	u.reader = newInputReader(in)
	// Type-ahead decodes and handles as soon as these start, so from here New
	// holds the lock like every public method (invariant 4): the startup frame
	// must not compose against a key-loop repaint, and the cell-size query
	// below must not interleave with one.
	u.mu.Lock()
	defer u.mu.Unlock()

	u.safeGo(u.reader.run)
	u.safeGo(u.readKeys)
	u.safeGo(u.watchSignals)
	u.safeGo(u.watchStatus)
	if u.images != ImageNone {
		u.safeGo(u.watchCells)
		// ask once, and the answer sizes every image this session draws
		u.render.query(cellSizeQuery)
	}
	if opts.OnEdit != nil {
		u.editCh = make(chan string, 1)
		u.safeGo(u.drainEdits) // runs OnEdit outside the UI lock
	}
	u.repaint()
	return u, nil
}

// guard wraps fn so a panic restores the terminal before it unwinds.
func (u *UI) guard(fn func()) func() {
	return func() {
		defer u.recoverClosed()
		fn()
	}
}

// guardArg is guard for a one-argument callback, such as a ToolStart done hook
// a caller runs on its own goroutine.
func guardArg[A any](u *UI, fn func(A)) func(A) {
	return func(a A) {
		defer u.recoverClosed()
		fn(a)
	}
}

// recoverClosed recovers a panic after restoring the terminal, then re-raises it.
func (u *UI) recoverClosed() {
	if p := recover(); p != nil {
		u.Close()
		panic(p)
	}
}

// safeGo runs fn in a goroutine, restoring the terminal before a panic unwinds.
func (u *UI) safeGo(fn func()) {
	go u.guard(fn)()
}

// afterSafe arms an afterDelay timer whose callback is wrapped by guard, so a
// panicking timer cannot leave the terminal raw. UI built without New still arms
// a real timer.
func (u *UI) afterSafe(delay time.Duration, fn func()) *time.Timer {
	after := u.afterDelay
	if after == nil {
		after = time.AfterFunc
	}
	return after(delay, u.guard(fn))
}

// SetTheme recolors the live block and everything committed after it. History
// already on screen keeps the colors it was written with.
func (u *UI) SetTheme(pal Palette) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.theme = NewTheme(u.theme.Profile, pal)
	u.streamOK, u.thinkOK = false, false // previews carry styled text
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

// CopySelection returns the highlighted picker row's clipboard payload. ok is
// false when no picker row carries one, which is also the cue for a host to
// ignore a ControlCopySelection that outlived its picker.
func (u *UI) CopySelection() (text string, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.act == nil {
		return "", false
	}
	p, isPick := u.act.it.(*pickState)
	if !isPick || len(p.matches) == 0 {
		return "", false
	}
	text = p.items[p.matches[p.cursor]].Copy
	return text, text != ""
}

// ErrTerminalClosed is returned by TerminalWriter's Write once Close restored
// the terminal, so an out-of-band writer stops painting into the shell.
var ErrTerminalClosed = errors.New("tui: terminal closed")

// TerminalWriter returns an out-of-band sink onto the UI's terminal, writes
// serialized with every frame under the UI lock. For escapes that must not
// interleave with painting (an OSC 52 clipboard write). Once the UI closes it
// refuses with ErrTerminalClosed, since the terminal belongs to the shell.
func (u *UI) TerminalWriter() io.Writer {
	return terminalWriter{u: u}
}

// terminalWriter adapts a UI into an io.Writer taking u.mu per write.
type terminalWriter struct {
	u *UI
}

func (w terminalWriter) Write(p []byte) (int, error) {
	u := w.u
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.closed {
		return 0, ErrTerminalClosed
	}
	u.render.rawWrite(string(p))
	return len(p), nil
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
	u.pastes = nil // the session is over, nothing can expand them now
	if u.editCh != nil {
		ch := u.editCh
		u.editCh = nil // handleKey's nil guard drops further edits instead of sending on a closed channel
		close(ch)      // ends drainEdits, its range loop exits cleanly
	}
	close(u.done)
	if len(u.deferred) > 0 {
		// a burst overlapping the end of a turn must not swallow committed output
		u.commitNowLocked(u.deferred)
		u.deferred = nil
	}
	u.render.close(u.inFd)
}

// SetDeferHistory makes alt mode keep committed rows while only live rows change.
func (u *UI) SetDeferHistory(v bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if r, ok := u.render.(*altRenderer); ok {
		r.deferHistory = v
	}
}

// Reset drops rendered state so a rewind can redraw just the current session.
// Where the renderer owns scrollback (alt mode) committed lines go too, and
// inline keeps the terminal's own scrollback but our buffers and live block reset.
// The next Replay paints the restored branch fresh.
func (u *UI) Reset() {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.closed {
		return
	}
	u.stopSpinner()
	u.cancelRewindLocked()
	u.thinkBuf.Flush()
	u.runs = nil
	u.textBuf = ""
	u.streaming = false
	u.textStart = false
	u.started = false // the next gap no longer needs a leading blank line
	u.lastBlank = false
	u.streamOK, u.thinkOK = false, false
	u.deferred = nil // held-back commits belong to the dropped state
	u.thinking = false
	u.busy = false
	u.activity = nil // transient rows are live-block only, a reset drops them
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
// segment. To update one part use SetModel or SetStatusSegment instead.
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
	Compact   int // where an auto-compaction would fire, 0 when unset
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

// SetInput replaces the editor buffer, e.g. to pre-fill a prompt after a rewind.
func (u *UI) SetInput(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.closed || u.act != nil { // never clobber an active interaction's input
		return
	}
	u.editor.SetValue(text)
	u.resetPromptNavLocked() // a host fill is never a recall step
	u.repaint()
}

// SetQueued replaces the dimmed pending-prompt rows shown above the input,
// oldest first. The driver (steer queue) owns the list, and empty clears.
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
	u.resetPromptNavLocked() // a host fill is never a recall step
	u.repaint()
}

// UserEcho commits a submitted message to history.
func (u *UI) UserEcho(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.gap()
	// flowReflow, not flowWrap: pre-wrapping it here would freeze the message at
	// the width it was sent at, and the terminal could never reflow it again.
	// Explicit newlines keep their continuation indent. Soft wraps are the
	// terminal's, exactly like the rest of committed output.
	u.commit(indentLines(u.theme.User.Wrap(text), u.theme.User.Wrap(userMarker), userContinue), flowReflow)
}

// UserImages commits the images submitted with a prompt, each drawn by the
// detected terminal protocol or named by its placeholder, below the echo.
func (u *UI) UserImages(ims []Image) {
	if len(ims) == 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.closed {
		return
	}
	u.gap()
	lines := make([]histLine, len(ims))
	for i, im := range ims {
		lines[i] = u.imageLine(im)
	}
	u.commitHist(lines)
}

// Image commits one image to history, drawn via the detected terminal image
// protocol or named honestly by a placeholder line. Sinks call it when a tool
// result or other event carries an image block.
func (u *UI) Image(im Image) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.closed {
		return
	}
	u.commitHist([]histLine{u.imageLine(im)})
}

// streamDelta runs one buffered delta through a commit: completed units drop from
// the live preview before they commit, so commit does not redraw them as a stale
// ghost below history. An uncompleted tail grows in place instead.
func (u *UI) streamDelta(committed, grew bool, commit func()) {
	if committed {
		u.repaint() // drop the just-committed rows from r.live before they redraw below history
	}
	commit()
	if grew {
		u.repaint() // extend the live tail as it grows, against the state the commit left
	}
}

// endStream commits a stream's remainder and clears its preview. Caller has emptied
// the buffer and cleared the flag, then repaint drops the stale rows from r.live before
// commit redraws them.
func (u *UI) endStream(rest string, commit func(string)) {
	if rest != "" {
		u.repaint() // drop the preview before committing its content
	}
	commit(rest)
	u.repaint() // clear any remaining preview rows
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
	whole := u.thinkBuf.Add(delta)
	committed := whole != ""
	grew := !committed && u.thinkBuf.Pending() != "" // a partial tail still streams
	u.streamDelta(committed, grew,
		func() { u.commit(styleLines(u.theme.Thinking, whole), flowReflow) })
}

// EndThinking flushes any partial reasoning line.
func (u *UI) EndThinking() {
	u.mu.Lock()
	defer u.mu.Unlock()

	rest := u.thinkBuf.Flush()
	u.thinking = false
	u.endStream(rest,
		func(r string) { u.commit(styleLines(u.theme.Thinking, r), flowReflow) })
}

// Text streams assistant output. Complete markdown blocks commit to history,
// and the partial remainder renders live above the input so a reply appears word
// by word instead of only at block boundaries.
func (u *UI) Text(delta string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.textBuf += delta
	u.streaming = true
	done, rest := splitCompleteBlocks(u.textBuf)
	u.textBuf = rest
	committed := len(done) > 0
	// a partial block still streams, and it is recomposed after a commit too so its
	// preview reserves the separator that commit just made necessary
	grew := len(rest) > 0
	u.streamDelta(committed, grew,
		func() { u.writeMarkdown(done) })
}

// EndText renders whatever remains of the current message and drops the preview.
func (u *UI) EndText() {
	u.mu.Lock()
	defer u.mu.Unlock()

	rest := u.textBuf
	u.textBuf = ""
	u.streaming = false
	u.endStream(rest, u.writeMarkdown)
	u.textStart = false
}

// Print renders a complete markdown document into history at once, the form
// /help uses. It is the synchronous equivalent of Text followed by EndText.
func (u *UI) Print(markdown string) {
	u.Text(markdown)
	u.EndText()
}

// streamingRows lays the tail of the in-progress markdown out into display rows
// for the live block, at the width and by the rule the block will be committed with.
func (u *UI) streamingRows(w int) []string {
	if !u.streaming || strings.TrimSpace(u.textBuf) == "" {
		return nil
	}
	rows := u.previewRows(w)
	if len(rows) == 0 {
		return nil
	}
	if !u.previewGap() {
		return rows
	}
	return append([]string{""}, rows...) // the separator writeMarkdown commits, or the block shifts down a row
}

// previewRows composes the buffered markdown tail into display rows, memoized
// between deltas so idle spinner frames skip the re-parse and re-wrap.
func (u *UI) previewRows(w int) []string {
	src := tailRunes(u.textBuf, streamPreviewRunes)
	if u.streamOK && u.streamKey == src && u.streamW == w {
		return u.streamRows
	}
	var rows []string
	for _, l := range renderPreview(u.theme, w, src) {
		if l.structured() {
			// structured lines carry no text. Lay them out or the preview
			// shows a blank row until the block commits.
			rows = append(rows, l.rows(w)...)
			continue
		}
		rows = append(rows, u.wrapPreview(l.text, w)...)
	}
	u.streamKey, u.streamW, u.streamRows, u.streamOK = src, w, rows, true
	return rows
}

// previewGap reports whether committing the buffered block opens with a blank
// line. Mirrors writeMarkdown's separator, and gap() behind it.
func (u *UI) previewGap() bool {
	return u.textStart || (u.started && !u.lastBlank)
}

// wrapPreview breaks a preview line the way this renderer commits it: inline
// hands the line to the terminal, alt wraps it itself.
func (u *UI) wrapPreview(line string, w int) []string {
	if u.mode == ModeInline {
		return hardWrap(line, w)
	}
	return wrapLine(line, w)
}

// tailRunes returns s when it holds at most n runes, else the last n of them,
// cut on a rune boundary so wrapping never sees half a sequence.
func tailRunes(s string, n int) string {
	if len(s) <= n { // every rune costs at least one byte
		return s
	}
	floor := max(len(s)-utf8.UTFMax*n, 0) // bounds the walk when starts run out (invalid UTF-8)
	i := len(s)
	for r := 0; i > floor && r < n; {
		i--
		if utf8.RuneStart(s[i]) {
			r++
		}
	}
	return s[i:]
}

// thinkingPreviewRows lays the pending partial reasoning line out into display
// rows for the live block, broken the way the committed line will be.
func (u *UI) thinkingPreviewRows(w int) []string {
	if !u.thinking || strings.TrimSpace(u.thinkBuf.Pending()) == "" {
		return nil
	}
	tail := tailRunes(u.thinkBuf.Pending(), thinkingPreviewRunes)
	if !u.thinkOK || u.thinkKey != tail || u.thinkW != w { // re-wrapped between deltas only
		rows := u.wrapPreview(tail, w)
		for i, r := range rows {
			rows[i] = u.theme.Thinking.Wrap(r)
		}
		u.thinkKey, u.thinkW, u.thinkRows, u.thinkOK = tail, w, rows, true
	}
	return u.thinkRows
}

// Output streams raw tool output for call id, committed a line at a time with no
// markdown parsing, so log and test output keep their exact shape. Only the first
// few lines reach history, and past that an activity row counts the rest as it runs.
func (u *UI) Output(id, delta string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	r := u.runLocked(id)
	if head := styleLines(u.theme.Dim, r.head.add(delta)); head != "" {
		u.commit(head, flowWrap)
	}
	if r.head.hidden() > 0 {
		u.setActivityLocked(outputKey+id, outputRow(r), unranked)
	}
}

// SetOutputFull marks call id's streamed head to show every line, bypassing
// outputHeadLines collapse. Ending the call clears it.
func (u *UI) SetOutputFull(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.runLocked(id).head.full = true
}

// EndOutput flushes and closes the calls a turn owns, the turn-end safety net.
// A full-mode call is left alone, since it outlives the turn.
func (u *UI) EndOutput() {
	u.mu.Lock()
	defer u.mu.Unlock()

	for _, r := range slices.Clone(u.runs) {
		if !r.head.full {
			u.endRunLocked(r.id)
		}
	}
}

// runLocked returns call id's render state, starting one when output arrives
// before (or without) its ToolStart. Caller holds the lock.
func (u *UI) runLocked(id string) *toolRun {
	if i := slices.IndexFunc(u.runs, func(r *toolRun) bool { return r.id == id }); i >= 0 {
		return u.runs[i]
	}
	r := &toolRun{id: id}
	u.runs = append(u.runs, r)
	return r
}

// endRunLocked commits call id's streamed head, its collapse line, and drops the
// call along with its activity row. Caller holds the lock.
func (u *UI) endRunLocked(id string) {
	i := slices.IndexFunc(u.runs, func(r *toolRun) bool { return r.id == id })
	if i < 0 {
		return
	}
	r := u.runs[i]
	if tail := styleLines(u.theme.Dim, r.head.flush()); tail != "" {
		u.commit(tail, flowWrap)
	}
	u.commitSummary(&r.head)
	u.setActivityLocked(outputKey+id, "", unranked)
	u.runs = slices.Delete(u.runs, i, i+1)
}

// toolLabel reports whether any named call is still running (the newest one),
// "" when none is. It drives spinner color and animation, not status text.
// Caller holds the lock.
func (u *UI) toolLabel() string {
	for i := len(u.runs) - 1; i >= 0; i-- {
		if u.runs[i].name != "" { // a run whose header has not landed yet names nothing
			return u.runs[i].name
		}
	}
	return ""
}

// outputRow renders the transient row a long-running tool shows past its head.
func outputRow(r *toolRun) string {
	name := r.name
	if name == "" {
		name = "tool"
	}
	return name + " · " + strconv.Itoa(r.head.hidden()) + " lines · " + strutil.HumanSize(int64(r.head.bytes))
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

// ToolStart commits call id's tool header to history. The running tool keeps the
// status-bar glyph animated and colored (SpinnerTool) while it runs, with no
// separate spinner row drawn above the input: the tool name rides only on the
// committed header. The returned function clears it and commits result, which may
// be empty when output was already streamed.
func (u *UI) ToolStart(id, name, label string) func(result string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	label = sanitizeRow(label) // feeds the committed header, since name is short and trusted
	u.runLocked(id).name = name
	u.gap()
	u.commit(u.theme.Accent.Wrap(toolMarker)+" "+u.theme.Dim.Wrap(label), flowReflow)
	u.spinner = 0
	u.syncSpinnerLocked()
	u.repaint()

	return guardArg(u, func(result string) {
		u.mu.Lock()
		defer u.mu.Unlock()

		// close this call's stream before showing its result, so calls sharing a turn
		// (an agent tool and a staged shell) each get their own head and summary.
		u.endRunLocked(id)
		if strings.TrimSpace(result) != "" {
			// a non-streaming tool's Display gets the identical head-plus-summary treatment.
			var h outputHead // add returns whole lines, flush picks up any trailing partial
			head := styleLines(u.theme.Dim, h.add(result))
			head += styleLines(u.theme.Dim, h.flush())
			if head != "" {
				u.commit(head, flowWrap)
			}
			u.commitSummary(&h)
		}
		// a busy turn keeps its glyph animated, tool color comes from syncSpinnerLocked
		u.syncSpinnerLocked()
		u.repaint()
	})
}

// Busy makes the status-bar glyph animate for as long as a turn is in flight, so
// there is always an indicator while the model thinks or works. The returned
// function returns it to its static resting frame.
func (u *UI) Busy() func() {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.busy = true
	u.spinner = 0
	u.syncSpinnerLocked()
	u.repaint()

	return u.guard(func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		if !u.busy {
			return
		}
		u.busy = false
		u.syncSpinnerLocked()
		u.repaint()
	})
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
// Sanitizing once here covers every committed text line (inline, alt's retained
// lines, plain), keeping SGR so colored tool output reads, dropping the motion
// and screen escapes caller text may carry. A table commits as a structured
// line with empty text, so its cells are cleaned where they are built instead
// (buildTable). After Close restored the terminal, late events from a drain
// still landing must not write to it.
func (u *UI) commitHist(lines []histLine) {
	if u.closed || len(lines) == 0 {
		return
	}
	u.flushNotice()               // a keyed notice stops being collapsible once anything follows
	lines = splitHistLines(lines) // histLine.text is single-line by construction
	for i, l := range lines {
		lines[i].text = sanitizeRow(strings.ReplaceAll(l.text, "\t", tabSpaces))
	}
	u.lastBlank = lines[len(lines)-1].text == ""
	u.started = true
	if u.resizing.Load() {
		// commit erases the live block too, so it waits out the burst with
		// repaints. Resize flushes these in order once the size settles.
		u.deferred = append(u.deferred, lines...)
		return
	}
	u.commitNowLocked(lines)
}

// commitNowLocked fits image geometry at the size in force right now, then
// commits, so burst-deferred images land at the settled size. Caller holds the lock.
func (u *UI) commitNowLocked(lines []histLine) {
	for _, l := range lines {
		if l.image != nil {
			u.fitImageLocked(l.image)
		}
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
	if u.closed || u.mode == ModePlain || u.resizing.Load() {
		return
	}
	w, h := u.render.size()
	// The live block is composed one column short of the terminal. A row that
	// fills the last column leaves the cursor in the deferred-wrap state, and
	// emulators disagree on whether that marks the line as continued. That is
	// what decides how a resize reflows it. Composing narrow (rather than
	// truncating at draw time) means nothing is cut off the editor or a dialog.
	if w > 1 {
		w--
	}

	// the status block is composed first so every budget below it is exact
	st := u.status
	frame := spinnerFrames[u.spinner%len(spinnerFrames)]
	if !u.busy && u.toolLabel() == "" {
		frame = spinnerFrames[0] // static resting frame when idle, bottom-left of the cell
	}
	st.Spinner = u.spinnerStyleLocked().Wrap(frame)
	statusRows := st.rows(u.theme, w)

	var rows []string // a screenful; pre-sizing wastes more than it saves
	if u.noticeText != "" {
		rows = append(rows, u.noticeText)
	}
	// In-progress reasoning streams live above the input, above the reply preview.
	// It yields by the same room rule, so only the tail rows render.
	if tr := u.thinkingPreviewRows(w); len(tr) > 0 {
		room := h - len(rows) - 1 - len(statusRows) - 1 // divider, status, input
		if room < len(tr) {
			tr = tr[len(tr)-max(room, 0):]
		}
		rows = append(rows, tr...)
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
			// the marker costs a preview row, not a block row, so the total stays room
			sr = sr[len(sr)-max(room-1, 0):]
			if room > 0 {
				sr = append([]string{u.theme.Dim.Wrap(previewElided)}, sr...)
			}
		}
		rows = append(rows, sr...)
	}
	// a narrow rule atop the prompt area sets it apart from committed and streamed
	// output. Row accounting stays exact because this is one more real row here.
	if w > 0 {
		style := u.theme.Dim
		if u.ruleFlash {
			style = u.theme.Accent
		}
		rows = append(rows, style.Wrap(strings.Repeat(ruleChar, w)))
	}
	offset := len(rows)

	// an active history search rides above the editor like completion. The two are
	// mutually exclusive by construction (opening one clears the other).
	if u.search != nil {
		// a history search needs more room than completion so the full prompt reads,
		// and the input keeps its own share below.
		searchRows := u.search.rows(u.theme, w, max(4, (h-1)*2/3))
		rows = append(rows, searchRows...)
		offset += len(searchRows)
	}

	// the completion list rides above the editor, shifting the caret offset
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
	// keeps whatever was typed and shows it again once the prompt resolves. The
	// status rows below it are reserved here so its own cap stays exact.
	var curRow, curCol int
	if u.act != nil {
		var iRows []string
		iRows, curRow, curCol = u.interactionRows(w, h-len(rows)-len(statusRows))
		rows = append(rows, iRows...)
	} else {
		maxRows := u.inputRows()
		var inputRows []string
		inputRows, curRow, curCol = u.editor.inputView(u.theme, w, maxRows)
		rows = append(rows, inputRows...)
	}
	rows = append(rows, statusRows...)

	// Last line of defence on the block's height. Every producer above budgets
	// itself, but a floor (search, completion) or an unlucky width can still
	// push the total past the screen, and a block taller than the screen scrolls
	// as it is drawn. That strands the previous frame's rows above it, one
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
// needs it. Never re-armed after Close, which stopped it.
// Caller holds the lock.
func (u *UI) syncSpinnerLocked() {
	if u.closed {
		return
	}
	if u.busy || u.toolLabel() != "" {
		u.startSpinner()
	} else {
		u.stopSpinner()
	}
}

// spinnerStyleLocked colors the glyph by what the turn is waiting on. Caller holds the lock.
func (u *UI) spinnerStyleLocked() Style {
	switch {
	case u.toolLabel() != "":
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
	u.mu.Lock()
	defer u.mu.Unlock()

	var dirty bool
	submit, dirty, quit = u.applyKey(k)
	if u.editCh != nil && !quit {
		u.notifyEditLocked(u.expandPastes(u.editor.Value()))
	}
	if dirty && !quit {
		u.repaint()
	}
	return submit, quit
}

// Insert inserts text at the caret, as if typed, for hosts that deliver
// content outside the key stream (an image token captured from the clipboard).
func (u *UI) Insert(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.closed {
		return
	}
	u.editor.Insert(text)
	u.repaint()
	if u.editCh != nil {
		u.notifyEditLocked(u.expandPastes(u.editor.Value()))
	}
}

// SetOnEdit installs an input-change callback after construction, for hosts that
// build their accounting state later than Options (e.g. main's driver). It may be
// called again to swap the callback. drainEdits reads it per iteration so the new
// one takes effect immediately.
func (u *UI) SetOnEdit(fn func(string)) {
	if fn == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.closed {
		return
	} else if u.editCh != nil && u.onEdit != nil {
		u.onEdit = fn // swap: keep the existing drain goroutine
		return
	}
	u.editCh = make(chan string, 1)
	u.onEdit = fn
	u.safeGo(u.drainEdits) // safeGo: a panicking host callback must not leave the terminal raw
}

// notifyEditLocked hands the current editor text to OnEdit when it changed since
// the last notification. Caller holds the lock and must have confirmed editCh is
// non-nil, and Close clears it before closing so a send here never hits a closed channel.
func (u *UI) notifyEditLocked(text string) {
	if u.onEdit == nil || text == u.lastNotified {
		return
	}
	u.lastNotified = text
	select {
	case u.editCh <- text:
	default: // a newer edit is already queued, drop this one (latest wins)
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
// the key loop, and nil disables the gesture.
func (u *UI) SetHistorySearch(fn func() []SearchItem) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.historySearch = fn
	u.resetPromptNavLocked() // the arrow fallback reads this source: a swapped list never resumes mid-browse
	if fn == nil {
		u.search = nil // clear any open overlay when the source is removed
	}
	u.repaint()
}

// SetPromptHistory installs the plain ↑/↓ recall source, scoped however the
// host chooses. An empty list falls back to the history-search source's texts.
// fn runs off the key loop; nil returns arrows to that fallback directly.
func (u *UI) SetPromptHistory(fn func() []string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.promptHistory = fn
	u.resetPromptNavLocked() // a swapped list must not resume mid-browse on stale state
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
	u.safeGo(func() { u.deliverSearch(fn()) }) // safeGo: a host source panic must not leave the terminal raw
}

// acceptSearchLocked fills the editor with the highlighted match (when there is
// one), caret on the matched text, and closes the overlay. It does not submit.
// The caller decides whether to fall through so a key like ↑/↓ can then browse history
// from that point. Caller holds the lock.
func (u *UI) acceptSearchLocked() {
	if it, ok := u.search.current(); ok {
		u.editor.SetValueAt(it.Text, u.search.matchOffset(it.Text))
		u.resetPromptNavLocked() // the match replaced whatever a previous recall held
	}
	u.search = nil
}

// promptLoadKey names the transient status segment shown while the ↑/↓ prompt
// list loads off the key loop.
const promptLoadKey = "history"

// ensurePromptNavLocked arms the ↑/↓ prompt list load once per navigation
// episode: the arrow source when installed, else the history-search source's
// texts. An empty arrow list also falls back, so a fresh session with no
// prompts of its own still recalls the search scope's entries. A provider may
// scan transcripts, so the load runs off the key loop like Ctrl+R's: while it
// is in flight arrows fall back to editor history, a transient status segment
// says why, and the result lands through deliverPromptNav. A nil or empty
// result leaves prompts nil, and arrows fall back to editor history
// navigation. Caller holds the lock.
func (u *UI) ensurePromptNavLocked() {
	if u.prompts != nil || u.promptLoad != 0 || u.promptEmpty {
		return // list cached, a load in flight, or the last one already came back empty
	}
	arrowFn, searchFn := u.promptHistory, u.historySearch
	if arrowFn == nil && searchFn == nil {
		return
	}
	u.promptSeq++
	gen := u.promptSeq
	u.promptLoad = gen
	u.setSegmentLocked(Segment{Key: promptLoadKey, Text: "loading history…"})
	u.safeGo(func() { u.deliverPromptNav(gen, loadPromptNav(arrowFn, searchFn)) })
}

// loadPromptNav resolves the recall list in the arrows' preference order: the
// arrow source first, then the search source's texts when it comes back empty.
func loadPromptNav(arrowFn func() []string, searchFn func() []SearchItem) []string {
	var ps []string
	if arrowFn != nil {
		ps = arrowFn()
	}
	if len(ps) == 0 && searchFn != nil {
		items := searchFn()
		ps = make([]string, 0, len(items))
		for _, it := range items {
			ps = append(ps, it.Text)
		}
	}
	return ps
}

// deliverPromptNav stores the loaded list when its generation still owns the
// slot; a reset or re-arm while it was in flight drops it instead. An empty
// result arms no reload until resetPromptNavLocked, so an empty scope never
// re-scans per arrow press.
func (u *UI) deliverPromptNav(gen uint64, ps []string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.closed {
		if gen == u.promptLoad { // resolve the slot when this delivery still owns it
			u.promptLoad = 0
		}
		return
	}
	if gen != u.promptLoad {
		return
	}
	u.promptLoad = 0
	u.setSegmentLocked(Segment{Key: promptLoadKey}) // empty Text removes the slot
	if len(ps) > 0 {
		u.prompts = ps
		u.promptIdx = -1 // at the live buffer, first ↑ recalls the newest prompt
	} else {
		u.promptEmpty = true
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

// resetPromptNavLocked disarms prompt-history recall everywhere: the recorded-prompt
// browse drops its cached list, position and held draft, and the editor fallback's
// history returns to the live buffer. Every buffer replacement that is not itself a
// recall step must call this, or an edge arrow restores stale state over fresh content.
// Caller holds the lock.
func (u *UI) resetPromptNavLocked() {
	u.prompts = nil
	u.promptIdx = -1
	u.stashP = ""
	u.promptEmpty = false
	u.cancelPromptLoadLocked()
	u.editor.ResetHistory()
}

// cancelPromptLoadLocked drops any in-flight prompt-list load and its segment.
func (u *UI) cancelPromptLoadLocked() {
	if u.promptLoad == 0 {
		return
	}
	u.promptLoad = 0
	u.setSegmentLocked(Segment{Key: promptLoadKey}) // empty Text removes the slot
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

// inputRows is the editor's visible-row budget: at most a fraction of the screen.
func (u *UI) inputRows() int {
	_, h := u.render.size()
	return max(1, (h-1)/maxInputRatio)
}

// applyKey mutates the editor for one key and reports whether it changed any
// rendered state. Caller holds the lock.
func (u *UI) applyKey(k key) (submit *string, dirty bool, quit bool) {
	// Shift+Tab and Shift+←/→ are out-of-band mode controls: they reach the
	// control channel even while a dialog or overlay owns the keyboard. Ctrl+X
	// is the picker copy gesture: it is only meaningful while an interaction
	// owns the keyboard, and the front end resolves the highlighted row.
	switch k.typ {
	case keyBackTab, keyShiftRight:
		u.emitControl(ControlModeCycle)
		return nil, false, false
	case keyShiftLeft:
		u.emitControl(ControlModeCycleBack)
		return nil, false, false
	case keyPickerCopy:
		if u.act != nil {
			u.emitControl(ControlCopySelection)
		}
		return nil, false, false
	}
	if u.act != nil {
		u.routeKey(k) // an active interaction sees every key before the editor
		return nil, false, false
	}
	// an open history search consumes keys ahead of the editor. Up
	// and Down select: they commit the highlighted prompt into the field, close the
	// overlay, but do not scroll on this same press. Subsequent arrows browse.
	if u.search != nil {
		switch k.typ {
		case keyUp, keyDown:
			u.acceptSearchLocked() // select + close only, no navigation here
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
			case searchAcceptMove:
				u.acceptSearchLocked() // commit, then this caret key edits the line below
			case searchPass:
				u.search = nil // close and let the editor handle this key below
			}
		}
	}
	// a menu consumes Tab/↑/↓/Enter/Esc before the editor, and a Tab-driven listing
	// only takes Esc. Every other key clears it on the way out below.
	if u.completion != nil && u.completion.accept(k) {
		consume, doSubmit := u.completion.key(k, &u.editor)
		if k.typ == keyEscape {
			u.completion = nil
			u.cancelRewindLocked()
			return nil, true, false
		}
		if consume && doSubmit {
			u.completion = nil
			v := u.editor.Submit()
			u.resetPromptNavLocked() // submitted or discarded, browsing never survives
			if strings.TrimSpace(v) != "" {
				e := u.expandPastes(v)
				return &e, true, false
			}
			return nil, true, false
		}
		if consume {
			u.refreshMenu() // narrow the menu against the text the accept changed
			return nil, true, false
		}
	}
	switch k.typ {
	case keyTab:
		u.startCompletion()
		return nil, true, false
	case keyReverseSearch:
		u.openSearchLocked()
	case keyRune:
		u.cancelRewindLocked() // typing ends the idle double-Esc window
		u.editor.Insert(k.text)
	case keyPaste:
		u.cancelRewindLocked()
		text := normalizeInput(k.text)
		if len(text) > pasteThreshold {
			// large paste: store the content and insert a placeholder so the input
			// block stays small. The placeholder is expanded before sending.
			u.pasteSeq++
			placeholder := pastePlaceholder(text, u.pasteSeq)
			u.pastes = append(u.pastes, pasteEntry{placeholder: placeholder, content: text})
			u.editor.Insert(placeholder)
		} else {
			u.editor.Insert(text)
		}
	case keyNewline:
		u.cancelRewindLocked()
		u.editor.Insert("\n")
	case keyEnter:
		u.completion = nil // a listing never holds a send hostage
		v := u.editor.Submit()
		u.resetPromptNavLocked() // submitted or discarded, browsing never survives
		if strings.TrimSpace(v) != "" {
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
	case keyAltUp:
		u.emitControl(ControlRecallQueued) // the driver updates the editor via SetInput/PrependInput
		return nil, false, false           // no repaint here: those methods repaint
	case keyClipboardPaste:
		// out-of-band like Shift+Tab: the driver probes the clipboard and may
		// insert an image token via SetInput
		u.emitControl(ControlClipboardImage)
		return nil, false, false
	case keyUp:
		// Cursor-first: move up through visual rows, and only a press already on
		// the very first character (or an empty buffer) recalls older history.
		// This holds even while browsing recorded prompts: a recalled multi-line
		// prompt is editable text whose caret must reach its start before Up steps
		// to the next older entry, and Down restores the live draft from there.
		if !u.editor.Up(u.editorWidth()) {
			// already on the first visual row.
			if u.editor.pos > 0 {
				// mid-text: jump to the prompt's beginning, and a second Up recalls history
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
				// mid-text: jump to the prompt's end, and a second Down recalls next
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
	case keyUndo:
		if !u.editor.Undo() { // nothing to restore: no visual change
			return nil, false, false
		}
	case keyRedraw:
		// the fallthrough repaint redraws the live block. Committed rows are the
		// terminal's and stay exactly as they are.
		u.render.resize()
	case keyPageUp:
		if u.mode == ModeInline {
			// inline has no viewport, so page the editor toward its head
			u.editor.PageUp(u.editorWidth(), u.inputRows())
		} else {
			u.render.scroll(u.page())
		}
	case keyPageDown:
		if u.mode == ModeInline {
			// page the editor toward its tail
			u.editor.PageDown(u.editorWidth(), u.inputRows())
		} else {
			u.render.scroll(-u.page())
		}
	case keyEscape:
		if u.editor.Value() != "" {
			// Esc clears the buffer rather than rewinding, dropping any half-armed
			// gesture so its deferred lone-Esc cannot fire after this press.
			u.cancelRewindLocked()
			u.editor.Clear()
			u.resetPromptNavLocked() // a cleared draft must not come back via ↓
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
			u.resetPromptNavLocked() // a cleared draft must not come back via ↓
		}
	case keyEOF:
		if u.editor.Value() == "" {
			u.emitControl(ControlEOF)
			return nil, false, false // no visual change
		} else {
			u.editor.DeleteForward()
		}
	}
	// an edit means composing again: drop a stale selection, then re-query so a
	// live menu narrows and a spent listing closes
	if u.completion != nil {
		u.completion.moved = false
	}
	u.refreshMenu()
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
// reach. A long drag freezes the live block until it pauses, which is cheap
// because the emulator is busy mangling the screen then anyway.
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
			// both gates go up before contending for the lock
			u.sigGen.Add(1)
			if u.mode == ModeInline {
				u.resizing.Store(true)
			}
			u.holdForResize()
			timer.Reset(resizeSettle)
		case <-timer.C:
			// A SIGWINCH queued behind this timer read means the burst is still
			// going. Keep debouncing rather than redraw mid-reflow.
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
// corrupted by an erase landing mid-reflow. Alt owns its screen and plain has
// none, so neither should pay the hold.
func (u *UI) holdForResize() {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.resizing.Store(u.mode == ModeInline)
	u.holdGen.Store(u.sigGen.Load()) // every bump so far is sequenced, settles may clear
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
	u.cursorRow() // drop replies to superseded probes, what remains answers this one
	u.cprPending = true
	u.render.probe()
	u.afterSafe(resizeProbeTimeout, func() { u.probeTimedOut(gen) })
}

// probeTimedOut arms the settled redraw when the terminal never answered,
// writing off every outstanding probe.
func (u *UI) probeTimedOut(gen int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.probesOut == 0 || gen != u.probeSeq {
		return // a newer burst owns the barrier, its own timeout releases it
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
	u.afterSafe(resizeDrawGrace, func() { u.drawSettled(gen) })
}

// drawSettled runs the settled redraw once the draw grace elapsed without a
// newer SIGWINCH. During continuous fast resizing every grace is invalidated
// before it fires, so no frame is emitted until a genuine pause. Frames and
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
		return // answers a superseded probe, the newest is still in flight
	}
	u.settleProbedLocked(u.probeSeq)
}

// cursorRow drains the terminal's cursor reports, returning the newest row and
// whether one arrived. The closed-channel branch is defensive, since a drain
// that ignored a close would spin. Caller holds the lock.
func (u *UI) cursorRow() (int, bool) {
	if u.reader == nil {
		return 0, false
	}
	var row int
	var got bool
	for {
		select {
		case r, open := <-u.reader.reports:
			if !open {
				return row, got
			}
			row, got = r, true
		default:
			return row, got
		}
	}
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
// picks up the new size (alt re-lays and repaints its whole viewport, while inline
// just re-reads it and redraws the live block), the live block is recomposed
// (which also drops any deferred rows from its preview, the Text/EndText ghost
// invariant), then held-back commits flush above it. Caller holds the lock.
func (u *UI) settleResizeLocked() {
	gen := u.sigGen.Load() // one read: a bump between check and store must leave the gate up
	if gen != u.holdGen.Load() {
		return // a signal bumped mid-settle, its own burst redraws
	}
	u.resizing.Store(false)
	u.drawGen.Store(gen) // drawing is caught up with every signal so far
	w, h := u.render.size()
	// a grow in one dimension with a shrink in neither cannot retire the park:
	// only shrinking rewraps content into more rows or takes rows away. A corner
	// drag that widens but shortens still re-anchors, and so does an equal-size
	// settle, whose burst may have narrowed and dragged back.
	known := u.settledW > 0 && u.settledH > 0
	grew := known && w >= u.settledW && h >= u.settledH && (w > u.settledW || h > u.settledH)
	u.settledW, u.settledH = w, h
	u.render.resize()
	// the reply may predate the reflow, so it is a best guess: the renderer
	// decides against its own geometry whether the block lost the screen bottom.
	// Draining is unconditional so a stale report never accumulates, while acting
	// on one needs a probe of our own behind it. Every settle re-decides, passing a
	// row of zero when it has no usable evidence: a pad the last settle raised
	// may still be pending on a frame the gate abandoned, and it can only be
	// applied against the geometry it was measured on.
	row, ok := u.cursorRow()
	if !ok || !u.cprPending || grew {
		row = 0
	}
	u.render.reanchor(row, u.started)
	u.cprPending = false
	u.repaint()
	if len(u.deferred) > 0 {
		lines := u.deferred
		u.deferred = nil
		u.commitNowLocked(lines)
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
