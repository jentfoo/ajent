package app

import (
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

// hubUI forwards the TUI's mutating surface and reports each call to the hub,
// so a control client sees every visible change the local terminal commits.
// The driver hands this wrapper to every consumer instead of the raw *tui.UI;
// anything the wrapper does not report is either a stream event (observed
// through the hub's sink) or invisible chrome. A nil terminal (serve mode)
// turns every forward into a report only: the hub stays the sole front end.
type hubUI struct {
	*tui.UI
	hub     *sessionHub
	dialogs *interactionBroker
}

// Notify commits a marked line and reports it.
func (u *hubUI) Notify(msg string, level tui.Level) {
	if u.UI != nil {
		u.UI.Notify(msg, level)
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

// NotifyKeyed commits a keyed notice and reports it with its key.
func (u *hubUI) NotifyKeyed(key, msg string, level tui.Level) {
	if u.UI != nil {
		u.UI.NotifyKeyed(key, msg, level)
	}
	u.hub.notice(key, msg, level)
}

// Print renders markdown into history and reports it.
func (u *hubUI) Print(markdown string) {
	if u.UI != nil {
		u.UI.Print(markdown)
	}
	u.hub.print(markdown)
}

// SetStatusSegment adds, replaces or removes a keyed segment and reports it.
func (u *hubUI) SetStatusSegment(seg tui.Segment) {
	if u.UI != nil {
		u.UI.SetStatusSegment(seg)
	}
	u.hub.setSegment(seg)
}

// SetModel updates the model labels and reports them. Capabilities are not
// part of the terminal surface, so the hub keeps its last copy; prefer
// SetModelOf where the full model is in hand.
func (u *hubUI) SetModel(name, short string, maxTokens int) {
	if u.UI != nil {
		u.UI.SetModel(name, short, maxTokens)
	}
	u.hub.setModelLabel(name, short, maxTokens)
}

// SetModelOf is SetModel with the full model, keeping capabilities in the
// hub's copy. Every live model switch routes through here.
func (u *hubUI) SetModelOf(m llm.Model) {
	if u.UI != nil {
		u.UI.SetModel(m.Key(), m.ShortName(), m.ContextWindow)
	}
	u.hub.setModel(m)
}

// SetActivity adds, replaces or removes a keyed activity row and reports it.
func (u *hubUI) SetActivity(key, text string) {
	if u.UI != nil {
		u.UI.SetActivity(key, text)
	}
	u.hub.setActivity(key, text, activityUnranked)
}

// SetActivityRanked is SetActivity with the producer's own stable order.
func (u *hubUI) SetActivityRanked(key, text string, rank int) {
	if u.UI != nil {
		u.UI.SetActivityRanked(key, text, rank)
	}
	u.hub.setActivity(key, text, rank)
}

// SetQueued replaces the pending-prompt rows and reports them.
func (u *hubUI) SetQueued(texts []string) {
	if u.UI != nil {
		u.UI.SetQueued(texts)
	}
	u.hub.setQueued(texts)
}

// SetIdle flips the prompt's rest state and reports it.
func (u *hubUI) SetIdle(idle bool) {
	if u.UI != nil {
		u.UI.SetIdle(idle)
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

// SetContext updates the context bar. Display only: context rides stream
// events and snapshots instead.
func (u *hubUI) SetContext(ci tui.ContextInfo) {
	if u.UI != nil {
		u.UI.SetContext(ci)
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
// host fill fires no edit callback.
func (u *hubUI) SetInput(text string) {
	if u.UI != nil {
		u.UI.SetInput(text)
	}
	u.hub.draft(text)
}

// PrependInput inserts text above the editor's draft and feeds the remembered
// draft the same way.
func (u *hubUI) PrependInput(text string) {
	if u.UI != nil {
		u.UI.PrependInput(text)
	}
	u.hub.prependDraft(text)
}

// Interactive reports whether a local editor exists to recover queued
// prompts into. Serve mode answers false: an interrupt drops them instead.
func (u *hubUI) Interactive() bool { return u.UI != nil }
