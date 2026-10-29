package app

import (
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
)

// wrapUI builds the driver's hubUI wrapper over a raw test UI, with no
// observers attached, for call sites that only need the forwarding surface.
func wrapUI(tb testing.TB, ui *tui.UI) *hubUI {
	tb.Helper()

	b := newInteractionBroker(ui, nil, nil)
	h := newSessionHub(hubDeps{ui: ui, dialogs: b, acct: tokens.New(llm.Model{})}, hubEmitters{})
	return h.ui
}

// emitLog records every hub emission in arrival order.
type emitLog struct {
	mu      sync.Mutex
	notices []string
	prints  []string
	echoes  []string
	images  [][]tui.Image
	segs    []tui.Segment
	models  []string
	idles   []bool
	spins   []bool
	acts    []string
	queues  [][]string
	drafts  []string
	heads   []string
}

// rowsSeen-style snapshots final assertions read; each copies under the lock.
func (l *emitLog) noticesSeen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.notices)
}

func (l *emitLog) printsSeen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.prints)
}

func (l *emitLog) echoesSeen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.echoes)
}

func (l *emitLog) segsSeen() []tui.Segment {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.segs)
}

func (l *emitLog) draftsSeen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.drafts)
}

func (l *emitLog) emitters() hubEmitters {
	return hubEmitters{
		Head:       func(id string) { l.mu.Lock(); l.heads = append(l.heads, id); l.mu.Unlock() },
		Echo:       func(text string) { l.mu.Lock(); l.echoes = append(l.echoes, text); l.mu.Unlock() },
		EchoImages: func(imgs []tui.Image) { l.mu.Lock(); l.images = append(l.images, imgs); l.mu.Unlock() },
		Notice: func(key, msg string, level tui.Level) {
			l.mu.Lock()
			l.notices = append(l.notices, key+"|"+msg)
			l.mu.Unlock()
		},
		Print:    func(markdown string) { l.mu.Lock(); l.prints = append(l.prints, markdown); l.mu.Unlock() },
		Segment:  func(seg tui.Segment) { l.mu.Lock(); l.segs = append(l.segs, seg); l.mu.Unlock() },
		Model:    func(key, short string, window int) { l.mu.Lock(); l.models = append(l.models, key); l.mu.Unlock() },
		Idle:     func(idle bool) { l.mu.Lock(); l.idles = append(l.idles, idle); l.mu.Unlock() },
		Spinning: func(on bool) { l.mu.Lock(); l.spins = append(l.spins, on); l.mu.Unlock() },
		Activity: func(key, text string, rank int) { l.mu.Lock(); l.acts = append(l.acts, key+":"+text); l.mu.Unlock() },
		Queued:   func(texts []string) { l.mu.Lock(); l.queues = append(l.queues, texts); l.mu.Unlock() },
		Draft:    func(text string) { l.mu.Lock(); l.drafts = append(l.drafts, text); l.mu.Unlock() },
	}
}

// pipeUI opens a plain-mode TUI over pipes whose output drains to a buffer,
// for wrapper tests that assert what the raw surface committed.
func pipeUI(t *testing.T) (*tui.UI, func() string) {
	t.Helper()

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	var mu sync.Mutex
	var sb strings.Builder
	go func() { _, _ = io.Copy(&lockedWriter{mu: &mu, sb: &sb}, outR) }()
	return ui, func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	sb *strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sb.Write(p)
}

func TestHubUIReportsSurfaceChanges(t *testing.T) {
	t.Parallel()

	term, out := pipeUI(t)
	log := &emitLog{}
	hub := newSessionHub(hubDeps{ui: term, active: llm.Model{}, acct: tokens.New(llm.Model{})}, log.emitters())
	ui := hub.ui

	ui.Notify("plain notice", tui.LevelWarn)
	ui.NotifyKeyed("subagent", "keyed", tui.LevelInfo)
	ui.Print("# printed")
	ui.SetStatusSegment(segment(segMCP, "mcp 2/2", "2/2"))
	ui.SetModelOf(llm.Model{Provider: "p", ID: "big", ContextWindow: 200000,
		Caps: llm.Capabilities{Images: true}})
	ui.SetActivityRanked("job:1", "reading", 1)
	ui.SetActivity("misc", "note")
	ui.SetQueued([]string{"waiting"})
	ui.UserEcho("typed line")

	require.Eventually(t, func() bool { return strings.Contains(out(), "typed line") },
		2*time.Second, 5*time.Millisecond)

	hub.mu.Lock()
	idle := hub.idle
	model := hub.model
	queued := hub.queued
	segs := len(hub.segments)
	acts := len(hub.activity)
	hub.mu.Unlock()

	assert.True(t, idle)
	assert.Equal(t, hubModel{Key: "p/big", Short: "big", Window: 200000,
		Caps: llm.Capabilities{Images: true}}, model)
	assert.Equal(t, []string{"waiting"}, queued)
	assert.Equal(t, 1, segs)
	assert.Equal(t, 2, acts)

	assert.Equal(t, []string{"|plain notice", "subagent|keyed"}, log.noticesSeen())
	assert.Equal(t, []string{"# printed"}, log.printsSeen())
	assert.Equal(t, []string{"typed line"}, log.echoesSeen())
	require.Len(t, log.segsSeen(), 1)
	assert.Equal(t, segMCP, log.segs[0].Key)
	assert.Equal(t, []string{"p/big"}, log.models)
	assert.Equal(t, []string{"job:1:reading", "misc:note"}, log.acts)
	require.Len(t, log.queues, 1)
	assert.Equal(t, []string{"waiting"}, log.queues[0])
}

// TestHubUILabelSetKeepsCaps pins the two model setters: a label-only SetModel
// keeps the capabilities the last full model recorded.
func TestHubUILabelSetKeepsCaps(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	hub := newSessionHub(hubDeps{ui: term, acct: tokens.New(llm.Model{})}, hubEmitters{})
	caps := llm.Capabilities{Images: true}

	hub.ui.SetModelOf(llm.Model{Provider: "p", ID: "big", ContextWindow: 1000, Caps: caps})
	hub.ui.SetModel("p/big-alt", "big", 2000)

	hub.mu.Lock()
	m := hub.model
	hub.mu.Unlock()
	assert.Equal(t, "p/big-alt", m.Key)
	assert.Equal(t, 2000, m.Window)
	assert.Equal(t, caps, m.Caps)
}

// TestHubUINilTerminalReportsOnly pins serve mode's shape: a wrapper with no
// terminal reports every mutation and renders nothing, never panicking.
func TestHubUINilTerminalReportsOnly(t *testing.T) {
	t.Parallel()

	log := &emitLog{}
	hub := newSessionHub(hubDeps{acct: tokens.New(llm.Model{})}, log.emitters())
	ui := hub.ui

	require.False(t, hub.Interactive())
	ui.Notify("plain", tui.LevelWarn)
	ui.NotifyKeyed("subagent", "keyed", tui.LevelInfo)
	ui.Print("# printed")
	ui.SetStatusSegment(segment(segPlan, "planning", "plan"))
	ui.SetModelOf(llm.Model{Provider: "p", ID: "m"})
	ui.SetActivity("job:1", "reading")
	ui.SetQueued([]string{"waiting"})
	ui.SetIdle(false)
	ui.UserEcho("typed line")
	ui.SetInput("filled")
	ui.PrependInput("recovered")

	assert.Equal(t, []string{"|plain", "subagent|keyed"}, log.noticesSeen())
	assert.Equal(t, []string{"# printed"}, log.printsSeen())
	assert.Equal(t, []string{"typed line"}, log.echoesSeen())
	require.Len(t, log.segsSeen(), 1)
	require.Len(t, log.models, 1)
	require.Len(t, log.acts, 1)
	require.Len(t, log.queues, 1)
	require.Equal(t, []bool{false}, log.idles)
}

func TestHubUISegmentRemoval(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	log := &emitLog{}
	hub := newSessionHub(hubDeps{ui: term, acct: tokens.New(llm.Model{})}, log.emitters())
	ui := hub.ui

	ui.SetStatusSegment(segment(segPlan, "implementing", "impl"))
	ui.SetStatusSegment(segment(segPlan, "", "")) // empty text removes

	hub.mu.Lock()
	_, present := hub.segments[segPlan]
	hub.mu.Unlock()
	assert.False(t, present)

	segs := log.segsSeen()
	require.Len(t, segs, 2)
	assert.Empty(t, segs[1].Text)
}

func TestHubUIDraftCoalescesAndHidesSecrets(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	log := &emitLog{}
	hub := newSessionHub(hubDeps{ui: term, acct: tokens.New(llm.Model{})}, log.emitters())

	hub.draft("typ")
	hub.draft("typing")
	hub.draft("secret: never emit")
	hub.draft("") // cleared before the debounce fires

	require.Eventually(t, func() bool {
		log.mu.Lock()
		defer log.mu.Unlock()
		return len(log.drafts) == 1
	}, 2*time.Second, 5*time.Millisecond)

	assert.Equal(t, []string{""}, log.draftsSeen())
}

func TestHubUIIdleAndSpinTrack(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	log := &emitLog{}
	hub := newSessionHub(hubDeps{ui: term, acct: tokens.New(llm.Model{})}, log.emitters())
	ui := hub.ui

	ui.SetIdle(false)
	hub.sink().TurnStart(agent.TurnInfo{})
	hub.sink().TurnEnd(agent.TurnResult{})
	hub.sink().TurnEnd(agent.TurnResult{}) // idempotent: no second emission

	log.mu.Lock()
	idles := log.idles
	log.mu.Unlock()
	assert.Equal(t, []bool{false}, idles)
	assert.Equal(t, []bool{true, false}, log.spins)
}
