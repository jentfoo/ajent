package app

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// newTestHub builds a hub over a raw test UI with no observers, for wiring
// tests that drive the control loop or submit paths.
func newTestHub(ui *tui.UI) *sessionHub {
	b := newInteractionBroker(ui, nil, nil)
	return newSessionHub(hubDeps{ui: ui, dialogs: b, acct: tokens.New(llm.Model{})}, hubEmitters{})
}

// hubRig is one submit-and-observe harness: a hub wired to a real router,
// stager and steer-queue shape, with recorded pump inputs and a sink observer.
type hubRig struct {
	hub    *sessionHub
	ui     *hubUI
	pump   chan pumpLine
	stager *command.Stager
	gate   *typingGate
	router *lineRouter

	mu     sync.Mutex
	lines  []pumpLine
	events []string
}

// newHubRig wires the router exactly as the driver does.
func newHubRig(t *testing.T) *hubRig {
	t.Helper()

	term, _ := pipeUI(t)
	hub := newTestHub(term)
	rig := &hubRig{hub: hub, ui: hub.ui, pump: make(chan pumpLine, 16), gate: &typingGate{}}
	rig.stager = command.NewStager(context.Background(), realTools(t), agent.NopSink{})
	t.Cleanup(rig.stager.Cancel)
	rig.router = &lineRouter{
		ui: hub.ui, stager: rig.stager, gate: rig.gate,
		send: func(pl pumpLine) {
			rig.mu.Lock()
			rig.lines = append(rig.lines, pl)
			rig.mu.Unlock()
		},
	}
	hub.submitLine = rig.router.submit
	hub.Register(rigObserver{r: rig})
	return rig
}

func (r *hubRig) recorded() []pumpLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lines)
}

func (r *hubRig) sinkEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// realTools builds a bash-capable registry for stagers that must run lines.
func realTools(t *testing.T) *tools.Registry {
	t.Helper()
	reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "hub-rig"})
	require.NoError(t, err)
	return reg
}

type rigObserver struct {
	agent.NopSink
	r *hubRig
}

func (o rigObserver) Text(d string) { o.log("text:" + d) }

func (o rigObserver) log(ev string) {
	o.r.mu.Lock()
	defer o.r.mu.Unlock()
	o.r.events = append(o.r.events, ev)
}

// TestHubSubmitRoutesLikeTheEditor covers routing parity: each line kind
// through Submit produces the same dispatch the editor path produces, minus
// editor-only side effects, including `!!` exclusion.
func TestHubSubmitRoutesLikeTheEditor(t *testing.T) {
	t.Parallel()

	submit := func(t *testing.T, line string, fromEditor bool) []pumpLine {
		t.Helper()
		rig := newHubRig(t)
		if fromEditor {
			rig.router.route(line, true)
		} else {
			rig.hub.Submit(line, "controller")
		}
		return rig.recorded()
	}

	t.Run("prompt_queues_for_pump", func(t *testing.T) {
		assert.Equal(t, []pumpLine{{kind: command.KindPrompt, rest: "hello world", origin: "controller"}},
			submit(t, "hello world", false))
		// same dispatch the editor produces, minus the origin provenance
		editor := submit(t, "hello world", true)
		require.Len(t, editor, 1)
		assert.Equal(t, command.KindPrompt, editor[0].kind)
		assert.Equal(t, "hello world", editor[0].rest)
		assert.Empty(t, editor[0].origin)
	})

	t.Run("command_queues_for_pump", func(t *testing.T) {
		assert.Equal(t, []pumpLine{{kind: command.KindCommand, rest: "model x"}},
			submit(t, "/model x", false))
		assert.Equal(t, submit(t, "/model x", false), submit(t, "/model x", true))
	})

	t.Run("shell_stages_untouched_by_source", func(t *testing.T) {
		// `!` lines stage rather than queue: an inert stager keeps the runs idle,
		// so only the prompt input differs between the two sources (none)
		for _, fromEditor := range []bool{false, true} {
			assert.Empty(t, submit(t, "!sleep 30", fromEditor))
		}
	})

	t.Run("excluded_shell_form_parses", func(t *testing.T) {
		// `!!` is a shell line the router hands the stager with its excluded flag;
		// the classification is what a controller inherits, pinned through ParseLine
		line := command.ParseLine("!!echo hidden")
		assert.Equal(t, command.KindShell, line.Kind)
		assert.Equal(t, "echo hidden", line.Rest)
		assert.True(t, line.Excluded)
		assert.Empty(t, submit(t, "!!sleep 30", false))
	})
}

// TestHubSubmitForeignGoroutine drives Submit from a non-driver goroutine,
// race-enabled, proving the submit surface is safe from any goroutine.
func TestHubSubmitForeignGoroutine(t *testing.T) {
	t.Parallel()

	rig := newHubRig(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rig.hub.Submit("from elsewhere", "controller")
	}()
	<-done

	require.Equal(t, []pumpLine{{kind: command.KindPrompt, rest: "from elsewhere", origin: "controller"}},
		rig.recorded())
}

// TestHubRegisterFanout drives a scripted turn through the hub sink and
// asserts registered observers see the stream events.
func TestHubRegisterFanout(t *testing.T) {
	t.Parallel()

	rig := newHubRig(t)
	st := &agent.State{Model: llm.Model{ID: "m"}, Tokens: tokens.New(llm.Model{ID: "m"})}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("hi")}}}, nil
		},
		Sinks: []agent.Sink{rig.hub.sink()},
		Env:   agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	require.NoError(t, ag.Prompt(t.Context(), agent.Input{Text: "go"}))

	require.Eventually(t, func() bool {
		return slices.Contains(rig.sinkEvents(), "text:hi")
	}, 5*time.Second, 5*time.Millisecond)
}

// TestHubObserverOrder pins registration-order fanout across two observers.
func TestHubObserverOrder(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	hub := newTestHub(term)
	var order []string
	var mu sync.Mutex
	name := func(n string) agent.Sink { return orderSink{n: n, mu: &mu, out: &order} }
	hub.Register(name("first"))
	hub.Register(name("second"))
	hub.sink().Text("x")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"first", "second"}, order)
}

// TestHubFanoutAfterRecorder pins the sink-list contract: with the recorder
// ahead of the hub, every event reaches the recorder before the hub's
// observers, the order the driver wires and 34a's clients rely on.
func TestHubFanoutAfterRecorder(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	hub := newTestHub(term)
	var order []string
	var mu sync.Mutex
	hub.Register(orderSink{n: "hub", mu: &mu, out: &order})

	st := &agent.State{Model: llm.Model{ID: "m"}, Tokens: tokens.New(llm.Model{ID: "m"})}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("hi")}}}, nil
		},
		Sinks: []agent.Sink{orderSink{n: "recorder", mu: &mu, out: &order}, hub.sink()},
		Env:   agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	require.NoError(t, ag.Prompt(t.Context(), agent.Input{Text: "go"}))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(order, "hub")
	}, 5*time.Second, 5*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for i := range order {
		if order[i] == "hub" {
			assert.Equal(t, "recorder", order[i-1])
		}
	}
}

type orderSink struct {
	agent.NopSink
	n   string
	mu  *sync.Mutex
	out *[]string
}

func (s orderSink) Text(string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.out = append(*s.out, s.n)
}

// TestHubInterruptMatchesKeypress pins the extracted interrupt body: called
// from a non-loop goroutine it stops a running turn exactly like the Esc
// keypress, and reports each priority tier.
func TestHubInterruptMatchesKeypress(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	hub := newTestHub(term)
	st := &agent.State{Model: llm.Model{ID: "test"}, Tokens: tokens.New(llm.Model{ID: "test"})}
	entered := make(chan struct{})
	release := make(chan struct{})
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: mainToolTurn()}}}, nil
		},
		Tools: singleToolSet{tool: holdBash{release: release, entered: entered}},
		Env:   agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	hub.ag = ag
	waitReady, takeReady := stagePair(t)
	hub.q = newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)
	stager := command.NewStager(context.Background(), nil, agent.NopSink{})
	hub.stager = stager
	t.Cleanup(stager.Cancel)

	assert.Equal(t, stopNothing, hub.Interrupt())

	errCh := make(chan error, 1)
	go func() { errCh <- ag.Prompt(t.Context(), agent.Input{Text: "run"}) }()
	<-entered

	done := make(chan interruptStop, 1)
	go func() { done <- hub.Interrupt() }()
	assert.Equal(t, stopTurn, <-done)

	select {
	case err := <-errCh:
		require.NoError(t, err) // a clean abort, never a failed turn
	case <-time.After(5 * time.Second):
		t.Fatal("the interrupt never stopped the turn")
	}
}

// TestHubModeCycleMatchesKeypress cycles through the hub, asserting the mode
// lands and the notice reaches the local surface in both directions.
func TestHubModeCycleMatchesKeypress(t *testing.T) {
	t.Parallel()

	term, out := pipeUI(t)
	hub := newTestHub(term)
	// an unsure classifier and a real registry stand in: the test only cycles modes
	b := wiredBarrier(tools.New())
	hub.barrier = b

	hub.ModeCycle(false)
	require.Eventually(t, func() bool {
		return strings.Count(out(), "permissions mode:") == 1
	}, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, permit.ModeAuto, b.Mode())
	// the permissions segment appears with the mode, hidden again on the way back
	hub.mu.Lock()
	_, segShown := hub.segments[segPermissions]
	hub.mu.Unlock()
	assert.True(t, segShown)

	hub.ModeCycle(true)
	require.Eventually(t, func() bool {
		return strings.Count(out(), "permissions mode:") == 2
	}, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, permit.ModeAllowRead, b.Mode())
	hub.mu.Lock()
	_, segShown = hub.segments[segPermissions]
	hub.mu.Unlock()
	assert.False(t, segShown)
}

// TestHubSnapshotTruth pins snapshot fields against their sources: tools from
// the registry, usage and context from the ledger, queued from the steer
// queue, running from the agent, permission from the barrier.
func TestHubSnapshotTruth(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	hub := newTestHub(term)
	st := &agent.State{Model: llm.Model{ID: "m", Provider: "p"},
		Tokens: tokens.New(llm.Model{ID: "m", Provider: "p"})}
	hub.ag = agent.New(st, agent.Options{})

	reg := tools.New()
	reg.Register(&stubTool{name: "alpha"}, true)
	reg.Register(&stubTool{name: "beta"}, false)
	reg.MarkReadOnly([]string{"alpha"})
	hub.tools = reg

	waitReady, takeReady := stagePair(t)
	q := newSteerQueue(hub.ui, func(int) {}, func() {}, waitReady, takeReady)
	hub.q = q
	q.hold(agent.Input{Text: "queued one"}, "queued one", 1)

	hub.barrier = wiredBarrier(reg) // default mode allow-read, the snapshot's expectation is auto

	// spend lands on the ledger's totals, read through the hub's captured pointer
	st.Tokens.RecordSpend("p/m", llm.Usage{Input: 120, Output: 30})
	st.Tokens.SetBase(40) // the context bar reads the estimate buckets

	hub.ui.SetStatusSegment(segment(segPlan, "planning", "plan"))
	hub.ui.SetModel("p/m", "m", 64000)

	snap := hub.Snapshot()
	assert.True(t, snap.Idle)
	assert.False(t, snap.Running)
	assert.Equal(t, 1, snap.Queued)
	assert.Equal(t, []string{"queued one"}, snap.QueuedTexts)
	assert.Equal(t, "allow-read", snap.Permission)
	assert.Equal(t, hubModel{Key: "p/m", Short: "m", Window: 64000}, snap.Model)
	assert.Equal(t, 120, snap.Usage.Total.Input)
	assert.Equal(t, 30, snap.Usage.Total.Output)
	assert.Equal(t, 1, snap.Usage.Turns)
	assert.Equal(t, 40, snap.Context.Used)
	assert.Equal(t, []hubTool{
		{Name: "alpha", Source: "builtin", Enabled: true, ReadOnly: true},
		{Name: "beta", Source: "builtin"},
	}, snap.Tools)
	require.Len(t, snap.Status.Segments, 1)
	assert.Equal(t, segPlan, snap.Status.Segments[0].Key)
	assert.Equal(t, "planning", snap.Status.Segments[0].Text)
}

// TestHubSnapshotWhileStreaming pins the mid-turn read: no agent state lock
// is taken, the cached ledger serves, and running/spinning hold.
func TestHubSnapshotWhileStreaming(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	hub := newTestHub(term)
	st := &agent.State{Model: llm.Model{ID: "test"}, Tokens: tokens.New(llm.Model{ID: "test"})}
	entered := make(chan struct{})
	release := make(chan struct{})
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: mainToolTurn()}}}, nil
		},
		Tools: singleToolSet{tool: holdBash{release: release, entered: entered}},
		Env:   agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
		Sinks: []agent.Sink{hub.sink()},
	})
	hub.ag = ag

	go func() { _ = ag.Prompt(t.Context(), agent.Input{Text: "run"}) }()
	<-entered

	snap := hub.Snapshot()
	assert.True(t, snap.Running)
	assert.True(t, snap.Spinning)
	close(release)
}

// TestHubSnapshotLedgerSwap pins the ledger capture rule: a rewind or fork
// that replaces the ledger must be visible to the next snapshot.
func TestHubSnapshotLedgerSwap(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	hub := newTestHub(term)
	st := &agent.State{Model: llm.Model{ID: "m"}, Tokens: tokens.New(llm.Model{ID: "m"})}
	ag := agent.New(st, agent.Options{})
	hub.ag = ag

	st.Tokens.RecordSpend("m", llm.Usage{Input: 10})
	require.Equal(t, 10, hub.Snapshot().Usage.Total.Input)

	fresh := tokens.New(llm.Model{ID: "m"})
	fresh.RecordSpend("m", llm.Usage{Input: 99})
	require.True(t, ag.WithState(func(s *agent.State) { s.Tokens = fresh }))
	hub.syncLedger()
	assert.Equal(t, 99, hub.Snapshot().Usage.Total.Input)
}

// TestHubHeadTracksSwitches pins the head surface: the open-time head seeds
// the snapshot, a rewind or fork move emits exactly once, and an unchanged
// head (a compaction switch) stays silent.
func TestHubHeadTracksSwitches(t *testing.T) {
	t.Parallel()

	w, err := session.Create(filepath.Join(t.TempDir(), "s.jsonl"),
		session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	_, err = w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "one")})
	require.NoError(t, err)
	entries, _, err := session.Read(w.Path())
	require.NoError(t, err)
	rec := &sessRec{w: w, rec: session.NewRecorder(w), snapshot: snapshot{entries: entries}}

	term, _ := pipeUI(t)
	log := &emitLog{}
	hub := newSessionHub(hubDeps{ui: term, dialogs: newInteractionBroker(term, nil, nil),
		acct: tokens.New(llm.Model{})}, log.emitters())
	hub.bindSession(rec)
	assert.Equal(t, w.Head(), hub.Snapshot().Head)

	// the driver's switch callback: a compaction fires it with the head unmoved
	hub.setHead(w.Head())
	log.mu.Lock()
	assert.Empty(t, log.heads)
	log.mu.Unlock()

	// a rewind moves the head onto the root entry, an earlier point
	rewound := entries[0].ID
	w.SetHead(rewound)
	hub.setHead(w.Head())

	log.mu.Lock()
	defer log.mu.Unlock()
	assert.Equal(t, []string{rewound}, log.heads)
	assert.Equal(t, rewound, hub.Snapshot().Head)
}

// TestHubBindSession pins the identity fields: root id from the transcript,
// name from its open-time parse.
func TestHubBindSession(t *testing.T) {
	t.Parallel()

	w, err := session.Create(filepath.Join(t.TempDir(), "s.jsonl"), session.SessionData{
		Version: session.Version(), Name: "named-one",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	rec := &sessRec{w: w, rec: session.NewRecorder(w)}
	entries, _, err := session.Read(w.Path())
	require.NoError(t, err)
	rec.snapshot.entries = entries // the open-time parse bindSession reads

	term, _ := pipeUI(t)
	hub := newTestHub(term)
	hub.bindSession(rec)

	snap := hub.Snapshot()
	assert.Equal(t, rec.w.Head(), snap.SessionID)
	assert.Equal(t, "named-one", snap.SessionName)
}
