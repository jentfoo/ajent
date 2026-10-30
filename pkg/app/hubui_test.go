package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-analyze/bulk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
)

// wrapUI builds the driver's hubUI wrapper over a raw test UI, with no
// observers attached, for call sites that only need the forwarding surface.
func wrapUI(tb testing.TB, ui *tui.UI) *hubUI {
	tb.Helper()

	b := newInteractionBroker(ui, nil, nil)
	h := newSessionHub(hubDeps{ui: ui, dialogs: b, acct: tokens.New(llm.Model{})})
	return h.ui
}

// emitLog records every control event a hub publishes, in seq order, via the
// same delivery seam a connection uses.
type emitLog struct {
	mu  sync.Mutex
	evs []ctl.Event
}

// attach subscribes the log to the hub's event fanout.
func (l *emitLog) attach(hub *sessionHub) {
	hub.evListen(func(ev ctl.Event) {
		l.mu.Lock()
		l.evs = append(l.evs, ev)
		l.mu.Unlock()
	})
}

// named copies the recorded events carrying one name.
func (l *emitLog) named(name string) []ctl.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bulk.SliceFilter(func(ev ctl.Event) bool { return ev.Name == name }, l.evs)
}

// noticesSeen renders notice events as key|text, matching the old log rows.
func (l *emitLog) noticesSeen() []string {
	var rows []string
	for _, ev := range l.named(ctl.EventNotice) {
		var d ctl.NoticeData
		if ev.Decode(&d) == nil {
			rows = append(rows, d.Key+"|"+d.Text)
		}
	}
	return rows
}

// printsSeen lists printed markdown blocks.
func (l *emitLog) printsSeen() []string {
	var rows []string
	for _, ev := range l.named(ctl.EventPrint) {
		var d ctl.PrintData
		if ev.Decode(&d) == nil {
			rows = append(rows, d.Markdown)
		}
	}
	return rows
}

// segsSeen lists status segment changes.
func (l *emitLog) segsSeen() []tui.Segment {
	var out []tui.Segment
	for _, ev := range l.named(ctl.EventStatus) {
		var d ctl.StatusData
		if ev.Decode(&d) == nil && d.Segment != nil {
			out = append(out, tui.Segment{Key: d.Segment.Key, Text: d.Segment.Text,
				Short: d.Segment.Short, Order: d.Segment.Order})
		}
	}
	return out
}

// modelsSeen lists model label changes.
func (l *emitLog) modelsSeen() []string {
	var out []string
	for _, ev := range l.named(ctl.EventStatus) {
		var d ctl.StatusData
		if ev.Decode(&d) == nil && d.Model != nil {
			out = append(out, d.Model.Key)
		}
	}
	return out
}

// actsSeen lists activity rows as key:text.
func (l *emitLog) actsSeen() []string {
	var out []string
	for _, ev := range l.named(ctl.EventActivity) {
		var d ctl.ActivityData
		if ev.Decode(&d) == nil {
			out = append(out, d.Key+":"+d.Text)
		}
	}
	return out
}

// queuesSeen lists queued-row replacements as id|text rows.
func (l *emitLog) queuesSeen() [][]ctl.QueueRow {
	var out [][]ctl.QueueRow
	for _, ev := range l.named(ctl.EventQueued) {
		var d ctl.QueuedData
		if ev.Decode(&d) == nil {
			out = append(out, d.Rows)
		}
	}
	return out
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
	hub := newSessionHub(hubDeps{ui: term, active: llm.Model{}, acct: tokens.New(llm.Model{})})
	log.attach(hub)
	ui := hub.ui

	ui.Notify("plain notice", tui.LevelWarn)
	ui.NotifyKeyed("subagent", "keyed", tui.LevelInfo)
	ui.Print("# printed")
	ui.SetStatusSegment(segment(segMCP, "mcp 2/2", "2/2"))
	ui.SetModelOf(llm.Model{Provider: "p", ID: "big", ContextWindow: 200000,
		Caps: llm.Capabilities{Images: true}})
	ui.SetActivityRanked("job:1", "reading", 1)
	ui.SetActivity("misc", "note")
	ui.SetQueued([]ctl.QueueRow{{ID: "row-1", Text: "waiting"}})
	ui.UserEcho("typed line") // local only: no echo event exists to observe

	require.Eventually(t, func() bool { return strings.Contains(out(), "typed line") },
		2*time.Second, 5*time.Millisecond)

	hub.mu.Lock()
	idle := hub.vm.idle
	model := hub.vm.model
	queued := hub.vm.queued
	segs := len(hub.vm.segments)
	acts := len(hub.vm.activity)
	ctx := hub.vm.context
	hub.mu.Unlock()

	assert.True(t, idle)
	assert.Equal(t, hubModel{Key: "p/big", Short: "big", Window: 200000,
		Caps: llm.Capabilities{Images: true}}, model)
	assert.Equal(t, []ctl.QueueRow{{ID: "row-1", Text: "waiting"}}, queued)
	assert.Equal(t, 1, segs)
	assert.Equal(t, 2, acts)
	assert.Zero(t, ctx.Used) // untouched here: context has its own flow

	assert.Equal(t, []string{"|plain notice", "subagent|keyed"}, log.noticesSeen())
	assert.Equal(t, []string{"# printed"}, log.printsSeen())
	require.Len(t, log.segsSeen(), 1)
	assert.Equal(t, segMCP, log.segsSeen()[0].Key)
	assert.Equal(t, []string{"p/big"}, log.modelsSeen())
	assert.Equal(t, []string{"job:1:reading", "misc:note"}, log.actsSeen())
	require.Len(t, log.queuesSeen(), 1)
	assert.Equal(t, []ctl.QueueRow{{ID: "row-1", Text: "waiting"}}, log.queuesSeen()[0])
}

// TestHubUILabelSetKeepsCaps pins the two model setters: a label-only SetModel
// keeps the capabilities the last full model recorded.
func TestHubUILabelSetKeepsCaps(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	hub := newSessionHub(hubDeps{ui: term, acct: tokens.New(llm.Model{})})
	caps := llm.Capabilities{Images: true}

	hub.ui.SetModelOf(llm.Model{Provider: "p", ID: "big", ContextWindow: 1000, Caps: caps})
	hub.ui.SetModel("p/big-alt", "big", 2000)

	hub.mu.Lock()
	m := hub.vm.model
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
	hub := newSessionHub(hubDeps{acct: tokens.New(llm.Model{})})
	log.attach(hub)
	ui := hub.ui

	require.False(t, hub.Interactive())
	ui.Notify("plain", tui.LevelWarn)
	ui.NotifyKeyed("subagent", "keyed", tui.LevelInfo)
	ui.Print("# printed")
	ui.SetStatusSegment(segment(segPlan, "planning", "plan"))
	ui.SetModelOf(llm.Model{Provider: "p", ID: "m"})
	ui.SetActivity("job:1", "reading")
	ui.SetQueued([]ctl.QueueRow{{ID: "row-2", Text: "waiting"}})
	ui.SetIdle(false)
	ui.UserEcho("typed line") // local only: no echo event exists to observe
	ui.SetInput("filled")
	ui.PrependInput("recovered")
	ui.Insert("token") // nil terminal: dropped, never a nil-receiver panic

	assert.Equal(t, []string{"|plain", "subagent|keyed"}, log.noticesSeen())
	assert.Equal(t, []string{"# printed"}, log.printsSeen())
	require.Len(t, log.segsSeen(), 1)
	require.Len(t, log.modelsSeen(), 1)
	require.Len(t, log.actsSeen(), 1)
	require.Len(t, log.queuesSeen(), 1)
}

func TestHubUISegmentRemoval(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	log := &emitLog{}
	hub := newSessionHub(hubDeps{ui: term, acct: tokens.New(llm.Model{})})
	log.attach(hub)
	ui := hub.ui

	ui.SetStatusSegment(segment(segPlan, "implementing", "impl"))
	ui.SetStatusSegment(segment(segPlan, "", "")) // empty text removes

	hub.mu.Lock()
	_, present := hub.vm.segments[segPlan]
	hub.mu.Unlock()
	assert.False(t, present)

	segs := log.segsSeen()
	require.Len(t, segs, 2)
	assert.Empty(t, segs[1].Text)
}

// TestHubUIShimCoversEveryMutation pins the shim rule: every TUI-visible
// mutation is declared on hubUI, so no consumer can reach the raw terminal
// past the hub's view model, and serve mode's nil terminal cannot leak into a
// call site. Local-only surfaces (editor wiring, dialogs, recall, theme setup)
// keep their own paths. This list is the mutation surface.
func TestHubUIShimCoversEveryMutation(t *testing.T) {
	t.Parallel()

	mutations := []string{
		"Notify", "NotifyLocal", "NotifyKeyed", "NotifyKeyedLocal", "Print",
		"SetStatusSegment", "SetModel", "SetModelOf",
		"SetActivity", "SetActivityRanked", "SetQueued", "SetIdle",
		"SetInput", "PrependInput", "Insert", "UserEcho", "UserImages",
		"SetTheme", "SetContext", "Busy", "Interactive",
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "hubui.go", nil, 0)
	require.NoError(t, err)
	declared := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			return true
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			return true
		}
		if ident, ok := star.X.(*ast.Ident); ok && ident.Name == "hubUI" {
			declared[fn.Name.Name] = true
		}
		return true
	})
	for _, name := range mutations {
		assert.True(t, declared[name], "hubUI must declare %s so the mutation routes through the hub", name)
	}
}

func TestHubUIIdleAndSpinTrack(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	log := &emitLog{}
	hub := newSessionHub(hubDeps{ui: term, acct: tokens.New(llm.Model{})})
	log.attach(hub)
	ui := hub.ui

	ui.SetIdle(false) // the flip publishes on the idle event
	hub.sink().TurnStart(agent.TurnInfo{})
	hub.sink().TurnEnd(agent.TurnResult{})
	hub.sink().TurnEnd(agent.TurnResult{}) // idempotent: no second emission

	spins := make([]bool, 0, 2)
	for _, ev := range log.named(ctl.EventStatus) {
		var d ctl.StatusData
		if ev.Decode(&d) == nil && d.Spinning != nil {
			spins = append(spins, *d.Spinning)
		}
	}
	assert.Equal(t, []bool{true, false}, spins)

	idles := make([]bool, 0, 1)
	for _, ev := range log.named(ctl.EventIdle) {
		var d ctl.IdleData
		if ev.Decode(&d) == nil {
			idles = append(idles, d.Idle)
		}
	}
	assert.Equal(t, []bool{false}, idles)
	hub.mu.Lock()
	assert.False(t, hub.vm.idle)
	assert.False(t, hub.vm.running) // the turn boundary tracked it back off
	hub.mu.Unlock()
}

// TestHubUIPaintsThroughSharedApplier pins the host half of the shared
// applier: reported mutations paint the local terminal through the same
// mapping an attached terminal applies to wire events, not past it.
func TestHubUIPaintsThroughSharedApplier(t *testing.T) {
	t.Parallel()

	term, out := pipeUI(t)
	hub := newSessionHub(hubDeps{ui: term, acct: tokens.New(llm.Model{})})
	ui := hub.ui
	require.NotNil(t, ui.ap)

	ui.Notify("shared notice", tui.LevelWarn)
	ui.NotifyKeyed("subagent", "one job", tui.LevelInfo)
	ui.Print("## printed block")
	ui.SetStatusSegment(segment(segMCP, "mcp 1/2", ""))
	ui.SetActivityRanked("job:1", "reading", 1)

	require.Eventually(t, func() bool {
		o := out()
		return strings.Contains(o, "shared notice") &&
			strings.Contains(o, "one job") &&
			strings.Contains(o, "printed block")
	}, 2*time.Second, 5*time.Millisecond)

	assert.Equal(t, map[string]bool{segMCP: true}, apKeys(ui.ap.segs))
	assert.Equal(t, map[string]bool{"job:1": true}, apKeys(ui.ap.acts))
}

// apKeys reduces an applier's tracked keys for comparison.
func apKeys(tracked map[string]struct{}) map[string]bool {
	out := make(map[string]bool, len(tracked))
	for key := range tracked {
		out[key] = true
	}
	return out
}
