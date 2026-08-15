package tui

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// Context block bounds: the subject elides to at most these rows and characters.
const (
	decisionContextRows  = 8
	decisionContextChars = 240
)

// DecisionRequest describes an approval prompt: the question, the subject shown
// above the options, and the choices offered.
type DecisionRequest struct {
	Prompt  string
	Context string // multi-line subject, styled as tool output and elided
	Options []Option
}

// DecisionResult reports the chosen option and whether the caller settled it
// rather than a keystroke.
type DecisionResult struct {
	Index    int
	External bool
}

// Decision is an open dialog. Wait blocks for the answer, Resolve settles it
// from the caller, and the first of the two wins.
type Decision struct {
	u   *UI
	st  *decisionState // nil when no dialog could be shown (plain mode or closed)
	p   *pending       // enqueued interaction; nil until Wait is meaningful
	err error          // failure captured at OpenDecision time, returned by Wait
}

// OpenDecision opens an approval dialog and returns a handle to it. The caller
// resolves it with Resolve, waits for the answer with Wait, or abandons it with
// Close (always safe via defer). Where no interactive terminal exists (plain mode
// or a closed UI) the returned handle's Wait reports ErrNoUI immediately.
func (u *UI) OpenDecision(req DecisionRequest) *Decision {
	d := &Decision{u: u}
	if len(req.Options) == 0 || u.mode == ModePlain {
		return d // no dialog can be shown; Wait reports ErrNoUI
	}
	st := &decisionState{prompt: req.Prompt, context: req.Context, options: slices.Clone(req.Options)}
	d.st = st
	p := newPending(st)
	if err := u.enqueue(p); err != nil {
		// enqueue only fails on a closed UI; report it as no one to ask (ErrNoUI),
		// matching Ask, rather than the cancellation ErrCancelled uses for teardown.
		d.err = ErrNoUI
		return d
	}
	d.p = p
	return d
}

// Wait blocks until the dialog resolves, ctx ends or the UI closes. Esc returns
// ErrCancelled (the caller decides what that means; for approval it is deny).
func (d *Decision) Wait(ctx context.Context) (DecisionResult, error) {
	switch {
	case d == nil || d.u == nil:
		return DecisionResult{}, ErrNoUI
	case d.st == nil:
		return DecisionResult{}, ErrNoUI // no dialog was shown
	case d.err != nil:
		return DecisionResult{}, d.err
	}
	if err := d.u.wait(ctx, d.p); err != nil {
		return DecisionResult{}, err
	}
	return d.st.result, nil
}

// Resolve settles an open dialog from the caller. The first resolver wins: a
// keystroke that already settled it makes this a no-op.
func (d *Decision) Resolve(index int) {
	if d == nil || d.u == nil || d.p == nil {
		return
	}
	d.u.mu.Lock()
	defer d.u.mu.Unlock()

	p := d.p
	st := p.it.(*decisionState)
	select {
	case <-p.done:
		return // someone else settled it; keep their result and summary
	default:
	}
	// only a winning resolver writes its own answer, before done closes so Wait sees it
	st.result = DecisionResult{Index: index, External: true} // read only after done closes
	if !p.resolve(nil) {                                     // a keystroke won concurrently; do not commit twice
		return
	}
	d.u.commitDecision(p)
}

// Close abandons an unresolved dialog, so defer d.Close() is always safe. A
// resolved or never-enqueued decision is left untouched.
func (d *Decision) Close() {
	if d == nil || d.u == nil || d.p == nil {
		return
	}
	d.u.abandon(d.p)
}

// commitDecision writes the one-line summary, dequeues and repaints. Caller holds
// the lock; only a winning resolver calls it.
func (u *UI) commitDecision(p *pending) {
	if s := p.it.summary(u.theme); s != "" {
		u.gap()
		u.commit(s, flowWrap)
	}
	u.dequeue(p)
	u.repaint()
}

// decisionState renders a context block above numbered options.
type decisionState struct {
	prompt  string
	context string
	options []Option
	cursor  int
	result  DecisionResult // written under u.mu before resolve; read after done closes
}

func (s *decisionState) rows(t Theme, width, maxRows int) ([]string, int, int) {
	out := []string{t.Accent.Wrap(s.prompt)}

	// elide the subject to its budget; render as many lines as room allows while
	// always leaving at least one option row below.
	ctxLines, cut := s.elideContext()
	availCtx := min(len(ctxLines), max(0, maxRows-len(out)-1))
	for _, l := range ctxLines[:availCtx] {
		out = append(out, truncateDisplay(t.Dim.Wrap(strings.TrimRight(l, " \t")), width))
	}

	// a dim marker reports the subject lines that were cut or did not fit
	if hidden := cut + len(ctxLines) - availCtx; hidden > 0 && len(out) < maxRows-1 {
		out = append(out, t.Dim.Wrap("…+"+strconv.Itoa(hidden)+" lines"))
	}

	listRows := max(1, maxRows-len(out))
	start, end := windowFor(s.cursor, len(s.options), listRows)
	for i := start; i < end; i++ {
		out = append(out, numberedOptionRow(t, s.options[i], i+1, i == s.cursor, width))
	}
	if end-start < len(s.options) && len(out) < maxRows {
		out = append(out, t.Dim.Wrap(selectIndent+moreLabel(len(s.options)-(end-start))))
	}
	return out, 0, 0
}

// elideContext splits the subject into up to decisionContextLines source lines whose
// total length stays under decisionContextChars. It returns the kept lines and how
// many input lines were dropped. The result is tool output: never passed through
// renderMarkdown.
func (s *decisionState) elideContext() ([]string, int) {
	var out []string
	total := 0
	cut := 0
	for _, ln := range strings.Split(s.context, "\n") {
		if len(out) >= decisionContextRows || total+len(ln) > decisionContextChars {
			cut++ // count dropped lines so the marker can name them
			continue
		}
		out = append(out, ln)
		total += len(ln)
	}
	return out, cut
}

func (s *decisionState) key(k key) (bool, error) {
	switch k.typ {
	case keyUp:
		s.cursor = wrapIndex(s.cursor-1, len(s.options))
	case keyDown:
		s.cursor = wrapIndex(s.cursor+1, len(s.options))
	case keyEnter:
		s.result = DecisionResult{Index: s.cursor}
		return true, nil
	case keyEscape, keyInterrupt:
		return true, ErrCancelled
	case keyRune:
		if n, err := strconv.Atoi(k.text); err == nil && n >= 1 && n <= min(len(s.options), interactionCap) {
			s.cursor = n - 1
			s.result = DecisionResult{Index: s.cursor}
			return true, nil
		}
	}
	return false, nil
}

func (s *decisionState) summary(t Theme) string {
	if len(s.options) == 0 || s.cursor >= len(s.options) {
		return ""
	}
	return t.Dim.Wrap(noticeMarker + " " + s.prompt + " " + s.options[s.cursor].Label)
}

// numberedOptionRow renders one option with a leading number, marked when it is
// the cursor. n is the option's one-based display position.
func numberedOptionRow(t Theme, o Option, n int, selected bool, width int) string {
	marker, style := selectIndent+strconv.Itoa(n)+" ", t.Dim
	if selected {
		marker, style = "> "+strconv.Itoa(n)+" ", t.Accent
	}
	line := style.Wrap(marker + o.Label)
	if o.Detail != "" {
		line += t.Dim.Wrap("  " + o.Detail)
	}
	return truncateDisplay(line, width)
}
