package app

import (
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
)

// hubUI is the shim between the session's consumers and the two surfaces a
// mutation reaches: the hub's view model (which publishes the wire event) and
// the local terminal, which repaints through the shared applier, the same
// mapping an attached terminal renders wire events with. A nil terminal (serve
// mode) skips the paint and the hub stays the sole front end, so no call site
// needs a nil check.
//
// The split is explicit in what each method forwards: every reported mutation
// applies locally through ap, then reports to the hub. Anything else on tui.UI
// that a shim method reaches past ap is local-only chrome by decision, not by
// omission: editor wiring (SetInput, PrependInput, Insert), dialog surfaces,
// theme, recall stores, and the NotifyLocal pair whose committed entries
// already carry their text to controllers.
type hubUI struct {
	*tui.UI
	hub     *sessionHub
	dialogs *interactionBroker
	ap      *applier // nil with no terminal: serve mode reports without painting
}

// Notify commits a marked line and reports it.
func (u *hubUI) Notify(msg string, level tui.Level) {
	if u.UI != nil {
		u.ap.notice("", msg, level)
	}
	u.hub.notice("", msg, level)
}

// NotifyLocal commits a marked line without reporting it: for notices whose
// committed entry already carries the text to controllers, so the wire never
// renders the same news twice.
func (u *hubUI) NotifyLocal(msg string, level tui.Level) {
	if u.UI != nil {
		u.UI.Notify(msg, level)
	}
}

// NotifyKeyedLocal is NotifyLocal for a keyed notice.
func (u *hubUI) NotifyKeyedLocal(key, msg string, level tui.Level) {
	if u.UI != nil {
		u.UI.NotifyKeyed(key, msg, level)
	}
}

// NotifyKeyed commits a keyed notice and reports it with its key.
func (u *hubUI) NotifyKeyed(key, msg string, level tui.Level) {
	if u.UI != nil {
		u.ap.notice(key, msg, level)
	}
	u.hub.notice(key, msg, level)
}

// Print renders markdown into history and reports it.
func (u *hubUI) Print(markdown string) {
	if u.UI != nil {
		u.ap.print(markdown)
	}
	u.hub.print(markdown)
}

// SetStatusSegment adds, replaces or removes a keyed segment and reports it.
func (u *hubUI) SetStatusSegment(seg tui.Segment) {
	if u.UI != nil {
		u.ap.segment(seg)
	}
	u.hub.setSegment(seg)
}

// SetModel updates the model labels and reports them. Capabilities are not
// part of the terminal surface, so the hub keeps its last copy; prefer
// SetModelOf where the full model is in hand.
func (u *hubUI) SetModel(name, short string, maxTokens int) {
	if u.UI != nil {
		u.ap.model(name, short, maxTokens)
	}
	u.hub.setModelLabel(name, short, maxTokens)
}

// SetModelOf is SetModel with the full model, keeping capabilities in the
// hub's copy. Every live model switch routes through here.
func (u *hubUI) SetModelOf(m llm.Model) {
	if u.UI != nil {
		u.ap.model(m.Key(), m.ShortName(), m.ContextWindow)
	}
	u.hub.setModel(m)
}

// SetActivity adds, replaces or removes a keyed activity row and reports it.
func (u *hubUI) SetActivity(key, text string) {
	u.SetActivityRanked(key, text, activityUnranked)
}

// SetActivityRanked is SetActivity with the producer's own stable order.
func (u *hubUI) SetActivityRanked(key, text string, rank int) {
	if u.UI != nil {
		u.ap.activity(key, text, rank)
	}
	u.hub.setActivity(key, text, rank)
}

// SetQueued replaces the pending-prompt rows and reports them: the local
// surface renders the real texts, the hub publishes rows with secrets redacted.
func (u *hubUI) SetQueued(rows []ctl.QueueRow) {
	if u.UI != nil {
		u.ap.queued(rows)
	}
	u.hub.setQueued(rows)
}

// SetIdle flips the prompt's rest state and reports it.
func (u *hubUI) SetIdle(idle bool) {
	if u.UI != nil {
		u.ap.idle(idle)
	}
	u.hub.setIdle(idle)
}

// UserEcho commits a submitted message. Local-only: the committed prompt
// reaches controllers as its transcript entry, never as an echo event.
func (u *hubUI) UserEcho(text string) {
	if u.UI != nil {
		u.UI.UserEcho(text)
	}
}

// UserImages commits submitted images beside a prompt. Local-only, like
// UserEcho: the entry.append carries them.
func (u *hubUI) UserImages(imgs []tui.Image) {
	if u.UI != nil {
		u.UI.UserImages(imgs)
	}
}

// SetTheme swaps the palette. Chrome only: nothing here is a control event.
func (u *hubUI) SetTheme(pal tui.Palette) {
	if u.UI != nil {
		u.UI.SetTheme(pal)
	}
}

// SetContext updates the context bar and reports it: the numbers are view
// model state, published on the context event family. The paint rides the
// applier's own sink mapping, like an attached terminal's context event.
func (u *hubUI) SetContext(ci tui.ContextInfo) {
	if u.UI != nil {
		u.ap.context(contextState(ci))
	}
	u.hub.setContext(contextState(ci))
}

// Insert types text at the caret for hosts that deliver content outside the
// key stream. The edit callback it fires feeds the draft mirror, like typing.
func (u *hubUI) Insert(text string) {
	if u.UI != nil {
		u.UI.Insert(text)
	}
}

// Busy lights the working glyph and reports how to release it. No terminal
// means nothing to light; the returned func still satisfies the compactor.
func (u *hubUI) Busy() func() {
	if u.UI == nil {
		return func() {}
	}
	return u.UI.Busy()
}

// SetInput replaces the editor buffer and feeds the remembered draft, since a
// host fill fires no edit callback. Local-only: drafts never leave the process.
func (u *hubUI) SetInput(text string) {
	if u.UI != nil {
		u.UI.SetInput(text)
	}
	u.hub.draft(text)
}

// PrependInput inserts text above the editor's draft and feeds the remembered
// draft the same way. Local-only, like SetInput.
func (u *hubUI) PrependInput(text string) {
	if u.UI != nil {
		u.UI.PrependInput(text)
	}
	u.hub.prependDraft(text)
}

// Interactive reports whether a local editor exists to recover queued
// prompts into. Serve mode answers false: an interrupt drops them instead.
func (u *hubUI) Interactive() bool { return u.UI != nil }

// contextState maps the terminal's context view onto the ledger's terms; the
// field lists are pinned equal by tui/sink's own conversion test.
func contextState(ci tui.ContextInfo) tokens.ContextState {
	return tokens.ContextState{Used: ci.Used, Window: ci.Window,
		Reserve: ci.Reserve, Compact: ci.Compact, Estimated: ci.Estimated}
}
