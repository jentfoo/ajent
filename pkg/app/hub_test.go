package app

import (
	"cmp"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-analyze/bulk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/ctl"
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
	return newSessionHub(hubDeps{ui: ui, dialogs: b, acct: tokens.New(llm.Model{})})
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
	rig.stager = command.NewStager(t.Context(), realTools(t), agent.NopSink{})
	t.Cleanup(rig.stager.Cancel)
	rig.router = &lineRouter{
		ui: hub.ui, stager: rig.stager, gate: rig.gate,
		send: func(pl pumpLine) {
			rig.mu.Lock()
			rig.lines = append(rig.lines, pl)
			rig.mu.Unlock()
		},
		trySend: func(pl pumpLine) bool {
			rig.router.send(pl)
			return true
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

// TestHubSubmit pins submit routing through the hub: editor-shaped lines
// dispatch like typed prompts, foreign-goroutine submits stay ordered, and a
// full queue refuses instead of dropping.
func TestHubSubmit(t *testing.T) {
	t.Run("routes_like_the_editor", func(t *testing.T) {
		submit := func(t *testing.T, line string, fromEditor bool) []pumpLine {
			t.Helper()
			rig := newHubRig(t)
			if fromEditor {
				rig.router.route(line, true)
			} else {
				require.NoError(t, rig.hub.Submit(line, "", "controller"))
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
	})

	t.Run("foreign_goroutine", func(t *testing.T) {
		rig := newHubRig(t)
		done := make(chan struct{})
		var serr error
		go func() {
			defer close(done)
			serr = rig.hub.Submit("from elsewhere", "", "controller")
		}()
		<-done
		require.NoError(t, serr)

		require.Equal(t, []pumpLine{{kind: command.KindPrompt, rest: "from elsewhere", origin: "controller"}},
			rig.recorded())
	})

	t.Run("full_queue_refuses", func(t *testing.T) {
		rig := newHubRig(t)
		rig.router.trySend = func(pumpLine) bool { return false }

		err := rig.hub.Submit("while wedged", "", "controller")
		require.ErrorIs(t, err, errPromptQueueFull)
		assert.Empty(t, rig.recorded())

		// the editor path is unaffected: it rides the blocking send
		rig.router.route("typed locally", true)
		assert.Equal(t, []pumpLine{{kind: command.KindPrompt, rest: "typed locally"}},
			rig.recorded())
	})
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

// sinkOrder returns the recorded fan-out sequence.
func sinkOrder(mu *sync.Mutex, out *[]string) []string {
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(*out)
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

	assert.Equal(t, []string{"first", "second"}, sinkOrder(&mu, &order))
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
	got := sinkOrder(&mu, &order)
	for i := range got {
		if got[i] == "hub" {
			assert.Equal(t, "recorder", got[i-1])
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
	hub.q = newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady, nil)
	stager := command.NewStager(t.Context(), nil, agent.NopSink{})
	hub.stager = stager
	t.Cleanup(stager.Cancel)

	assert.Equal(t, stopNothing, hub.Interrupt())

	errCh := make(chan error, 1)
	go func() { errCh <- ag.Prompt(t.Context(), agent.Input{Text: "run"}) }()
	<-entered

	done := make(chan interruptStop, 1)
	go func() { done <- hub.Interrupt() }()
	assert.Equal(t, stopTurn, <-done)

	err := <-errCh
	require.NoError(t, err) // a clean abort, never a failed turn
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
	_, segShown := hub.vm.segments[segPermissions]
	mode := hub.vm.permission
	hub.mu.Unlock()
	assert.True(t, segShown)
	assert.Equal(t, permit.ModeAuto.String(), mode)

	hub.ModeCycle(true)
	require.Eventually(t, func() bool {
		return strings.Count(out(), "permissions mode:") == 2
	}, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, permit.ModeAllowRead, b.Mode())
	hub.mu.Lock()
	_, segShown = hub.vm.segments[segPermissions]
	mode = hub.vm.permission
	hub.mu.Unlock()
	assert.False(t, segShown)
	assert.Equal(t, permit.ModeAllowRead.String(), mode)
}

// TestHubSnapshot pins the snapshot contract: at-rest truth, mid-turn
// streaming shape, ledger swaps on model change, event coverage for every field,
// and mutation consistency under concurrent readers.
func TestHubSnapshot(t *testing.T) {
	t.Run("truth", func(t *testing.T) {
		term, _ := pipeUI(t)
		hub := newTestHub(term)
		st := &agent.State{Model: llm.Model{ID: "m", Provider: "p"},
			Tokens: tokens.New(llm.Model{ID: "m", Provider: "p"})}
		hub.ag = agent.New(st, agent.Options{})

		reg := tools.New()
		reg.Register(&stubTool{name: "alpha"}, true)
		reg.Register(&stubTool{name: "beta"}, false)
		reg.MarkReadOnly([]string{"alpha"})
		hub.setToolsRegistry(reg)

		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(hub.ui, func(int) {}, func() {}, waitReady, takeReady, nil)
		hub.q = q
		q.hold(agent.Input{Text: "queued one", QueueIDs: []string{"row-1"}}, "queued one", 1)

		b := wiredBarrier(reg) // default mode allow-read, the snapshot's expectation is auto
		hub.barrier = b
		showPermissionIndicator(hub.ui, b)

		// spend lands on the ledger's totals, read through the hub's captured pointer
		st.Tokens.RecordSpend("p/m", llm.Usage{Input: 120, Output: 30})
		st.Tokens.SetBase(40)               // the context bar reads the estimate buckets
		hub.setContext(st.Tokens.Context()) // the view model's context copy

		hub.ui.SetStatusSegment(segment(segPlan, "planning", "plan"))
		hub.ui.SetModel("p/m", "m", 64000)

		snap := hub.Snapshot()
		assert.True(t, snap.Idle)
		assert.False(t, snap.Running)
		assert.Equal(t, 1, snap.Queued)
		assert.Equal(t, []ctl.QueueRow{{ID: "row-1", Text: "queued one"}}, snap.QueuedRows)
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
	})

	t.Run("while_streaming", func(t *testing.T) {
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
	})

	t.Run("ledger_swap", func(t *testing.T) {
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
	})
}

// TestHubHeadTracks pins setHead's two write paths: surface switches and
// recorded appends both move the head without disturbing live state.
func TestHubHeadTracks(t *testing.T) {
	t.Run("tracks_switches", func(t *testing.T) {
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
			acct: tokens.New(llm.Model{})})
		log.attach(hub)
		hub.bindSession(rec)
		assert.Equal(t, w.Head(), hub.Snapshot().Head)

		// the driver's switch callback: a compaction fires it with the head unmoved
		hub.setHead(w.Head(), rewindCauseRewind)
		assert.Empty(t, log.named(ctl.EventBranchChanged))

		// a rewind moves the head onto the root entry, an earlier point
		rewound := entries[0].ID
		w.SetHead(rewound)
		hub.setHead(w.Head(), rewindCauseRewind)

		moved := log.named(ctl.EventBranchChanged)
		require.Len(t, moved, 1)
		var bd ctl.BranchChangedData
		require.NoError(t, moved[0].Decode(&bd))
		assert.Equal(t, rewound, bd.Head)
		assert.Equal(t, rewound, hub.Snapshot().Head)
	})

	t.Run("tracks_appends", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := session.Create(p, session.SessionData{Version: session.Version()})
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		entries, _, err := session.Read(p)
		require.NoError(t, err)
		root := entries[0].ID
		rec := &sessRec{w: w, rec: session.NewRecorder(w), snapshot: snapshot{entries: entries}}

		term, _ := pipeUI(t)
		log := &emitLog{}
		hub := newSessionHub(hubDeps{ui: term, dialogs: newInteractionBroker(term, nil, nil),
			acct: tokens.New(llm.Model{})})
		log.attach(hub)
		hub.bindSession(rec) // binds the root, before any append

		e1, aerr := w.Append(session.TypeMessage,
			session.MessageData{Message: llm.Text(llm.RoleUser, "one")})
		require.NoError(t, aerr)
		hub.noteAppend(e1.ID) // what entryAppended fires per append
		assert.Equal(t, e1.ID, hub.Snapshot().Head)

		// rewind back onto the bind-time head: a real move, must publish
		w.SetHead(root)
		hub.setHead(w.Head(), rewindCauseRewind)
		moved := log.named(ctl.EventBranchChanged)
		require.Len(t, moved, 1)
		var bd ctl.BranchChangedData
		require.NoError(t, moved[0].Decode(&bd))
		assert.Equal(t, root, bd.Head)
		assert.Equal(t, e1.ID, bd.From)
	})
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

// TestHubQueueRowsRedactSecrets pins the parity rule: secret-prefixed queue
// rows lose their text on the wire but keep their id, so the submitting surface
// still resolves its pending row; the local surface keeps the real texts.
func TestHubQueueRowsRedactSecrets(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	log := &emitLog{}
	hub := newSessionHub(hubDeps{ui: term, dialogs: newInteractionBroker(term, nil, nil),
		acct: tokens.New(llm.Model{})})
	log.attach(hub)
	ui := hub.ui

	rows := []ctl.QueueRow{{ID: "row-1", Text: "visible prompt"}, {ID: "row-2", Text: "secret: hunter2"}}
	ui.SetQueued(rows)
	hub.queueDroppedCause("interrupt", rows)

	require.Eventually(t, func() bool { return len(log.queuesSeen()) == 1 },
		2*time.Second, 5*time.Millisecond)
	assert.Equal(t, []ctl.QueueRow{{ID: "row-1", Text: "visible prompt"}, {ID: "row-2"}}, log.queuesSeen()[0])

	require.Eventually(t, func() bool { return len(log.named(ctl.EventQueueDropped)) == 1 },
		2*time.Second, 5*time.Millisecond)
	var d ctl.QueueDroppedData
	require.NoError(t, log.named(ctl.EventQueueDropped)[0].Decode(&d))
	assert.Equal(t, []ctl.QueueRow{{ID: "row-1", Text: "visible prompt"}, {ID: "row-2"}}, d.Rows)

	// the remembered copy stays whole for snapshots of the local truth
	hub.mu.Lock()
	queued := hub.vm.queued
	hub.mu.Unlock()
	assert.Equal(t, rows, queued)
}

// TestHubSnapshotEventCoverage pins the event/snapshot contract
// (control-design.md): every hubSnapshot field either has a change event that
// announces it or sits on the poll-only list the design names, so a rendering
// surface can tell which facts events carry and which only a snapshot reads.
func TestHubSnapshotEventCoverage(t *testing.T) {
	t.Parallel()

	// eventCovered names the event(s) that announce each field's changes.
	eventCovered := map[string]string{
		"SessionName":  ctl.EventSessionRenamed,
		"Head":         ctl.EventBranchChanged + ", " + ctl.EventEntryAppend,
		"Model":        ctl.EventStatus,
		"Context":      ctl.EventContext,
		"Running":      ctl.EventTurnStart + ", " + ctl.EventTurnEnd,
		"Spinning":     ctl.EventStatus,
		"Idle":         ctl.EventIdle,
		"Queued":       ctl.EventQueued,
		"QueuedRows":   ctl.EventQueued,
		"Activity":     ctl.EventActivity,
		"Status":       ctl.EventStatus,
		"Permission":   ctl.EventPermission,
		"Tools":        ctl.EventTools,
		"Interactions": ctl.EventInteractionPending + ", " + ctl.EventInteractionSettled,
	}
	// pollOnly names why no event announces the field; the design doc carries
	// the same list.
	pollOnly := map[string]string{
		"SessionID": "static: identity never changes",
		"Workspace": "static: fixed for the run",
		"Cwd":       "static: fixed for the run",
		"Usage": "ledger totals accumulate between snapshots; the read is the " +
			"documented WithState read-after-unlock",
	}

	fields := make(map[string]bool)
	for _, f := range reflect.VisibleFields(reflect.TypeOf(hubSnapshot{})) {
		fields[f.Name] = true
		_, viaEvent := eventCovered[f.Name]
		_, viaPoll := pollOnly[f.Name]
		assert.True(t, viaEvent || viaPoll,
			"field %s is neither event-covered nor poll-only: name its coverage here and in control-design.md", f.Name)
	}
	for name := range eventCovered {
		assert.True(t, fields[name], "event-covered field %s no longer exists on hubSnapshot", name)
		_, viaPoll := pollOnly[name]
		assert.False(t, viaPoll, "field %s is both event-covered and poll-only", name)
	}
	for name := range pollOnly {
		assert.True(t, fields[name], "poll-only field %s no longer exists on hubSnapshot", name)
	}
}

// TestHubToolsPublishOnChange pins the tool rows' flow: installing a registry
// seeds the view model, and every later reshape fires the registry's change
// hook and republishes the rows without any per-site wiring.
func TestHubToolsPublishOnChange(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	log := &emitLog{}
	hub := newTestHub(term)
	log.attach(hub)

	reg := tools.New()
	reg.Register(&stubTool{name: "alpha"}, true)
	reg.Register(&stubTool{name: "beta"}, true)
	hub.setToolsRegistry(reg)

	require.Eventually(t, func() bool { return len(log.named(ctl.EventTools)) == 1 },
		2*time.Second, 5*time.Millisecond)
	var td ctl.ToolsData
	require.NoError(t, log.named(ctl.EventTools)[0].Decode(&td))
	require.Len(t, td.Rows, 2)
	assert.True(t, td.Rows[0].Enabled)

	reg.SetEnabled([]string{"alpha"})
	require.Eventually(t, func() bool { return len(log.named(ctl.EventTools)) == 2 },
		2*time.Second, 5*time.Millisecond)
	require.NoError(t, log.named(ctl.EventTools)[1].Decode(&td))
	enabled := bulk.SliceFilter(func(r ctl.ToolState) bool { return r.Enabled }, td.Rows)
	require.Len(t, enabled, 1)
	assert.Equal(t, "alpha", enabled[0].Name)

	snap := hub.Snapshot()
	require.Len(t, snap.Tools, 2)
	assert.False(t, snap.Tools[1].Enabled)
}

// TestHubContextMirrorsSink pins the context flow: the sink call records the
// numbers on the view model and publishes the event once, from the hub.
func TestHubContextMirrorsSink(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	log := &emitLog{}
	hub := newTestHub(term)
	log.attach(hub)

	want := tokens.ContextState{Used: 1200, Window: 64000, Reserve: 4000, Compact: 48000}
	hub.sink().Context(want)

	require.Eventually(t, func() bool { return len(log.named(ctl.EventContext)) == 1 },
		2*time.Second, 5*time.Millisecond)
	hub.mu.Lock()
	got := hub.vm.context
	hub.mu.Unlock()
	assert.Equal(t, want, got)
	assert.Equal(t, want.Used, hub.Snapshot().Context.Used)
}

// TestHubSnapshotConsistentUnderMutation hammers the view model setters
// against concurrent SnapshotAt reads: every snapshot must be one internally
// consistent state (counts matching their rows, the status row matching its
// inputs, rows in display order) whatever interleaving produced it.
func TestHubSnapshotConsistentUnderMutation(t *testing.T) {
	t.Parallel()

	term, _ := pipeUI(t)
	hub := newTestHub(term)
	reg := tools.New()
	reg.Register(&stubTool{name: "alpha"}, true)
	hub.setToolsRegistry(reg)

	// several setters touch the same fields, so readers interleave real races.
	// The closures pin their arguments so a later iteration cannot rewrite them.
	mutate := make([]func(), 0, 19)
	for _, spec := range []struct{ key, text, short string }{
		{segPlan, "planning", "plan"}, {segMCP, "mcp 1/2", "1/2"},
	} {
		key, text, short := spec.key, spec.text, spec.short
		mutate = append(mutate,
			func() { hub.setSegment(segment(key, text, short)) },
			func() { hub.setSegment(tui.Segment{Key: key}) })
	}
	for _, a := range []struct {
		key, text string
		rank      int
	}{{"job:1", "reading", 1}, {"misc", "note", activityUnranked}, {"job:1", "", 0}} {
		act := a
		mutate = append(mutate, func() { hub.setActivity(act.key, act.text, act.rank) })
	}
	for _, r := range [][]ctl.QueueRow{
		{{ID: "row-1", Text: "one"}, {ID: "row-2", Text: "two"}},
		{{ID: "row-3", Text: "three"}},
		nil,
	} {
		rows := r
		mutate = append(mutate, func() { hub.setQueued(rows) })
	}
	mutate = append(mutate,
		func() { hub.setIdle(true) },
		func() { hub.setIdle(false) },
		func() { hub.setTurnLive(true) },
		func() { hub.setTurnLive(false) },
		func() { hub.setModelLabel("p/m", "m", 64000) },
		func() { hub.setPermissionMode(permit.ModeAuto) },
		func() { hub.setPermissionMode(permit.ModeAllowRead) },
		func() { hub.refreshTools() },
		func() { hub.setContext(tokens.ContextState{Used: 100, Window: 64000, Reserve: 4000}) },
	)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				mutate[(n+j*4)%len(mutate)]()
			}
		}(i)
	}

	// validate names the invariant each check pins, on the snapshot as a whole
	validate := func(snap hubSnapshot) []string {
		var bad []string
		if snap.Queued != len(snap.QueuedRows) {
			bad = append(bad, "queued count disagrees with its rows")
		}
		if snap.Running != snap.Spinning { // both track the turn boundary
			bad = append(bad, "running and spinning disagree")
		}
		if snap.Spinning != (snap.Status.Spinner != "") {
			bad = append(bad, "spinner disagrees with spinning")
		}
		if snap.Status.Model != snap.Model.Key || snap.Status.ModelShort != snap.Model.Short ||
			snap.Status.MaxTokens != snap.Model.Window {
			bad = append(bad, "status labels disagree with the model copy")
		}
		if snap.Status.Tokens != snap.Context.Used || snap.Status.Reserve != snap.Context.Reserve ||
			snap.Status.Compact != snap.Context.Compact || snap.Status.Estimated != snap.Context.Estimated {
			bad = append(bad, "status numbers disagree with the context copy")
		}
		if !slices.IsSortedFunc(snap.Status.Segments, func(a, b tui.Segment) int {
			if c := cmp.Compare(a.Order, b.Order); c != 0 {
				return c
			}
			return cmp.Compare(a.Key, b.Key)
		}) {
			bad = append(bad, "segments out of display order")
		}
		if !slices.IsSortedFunc(snap.Activity, func(a, b hubActivity) int {
			return cmp.Compare(a.Rank, b.Rank)
		}) {
			bad = append(bad, "activity out of rank order")
		}
		return bad
	}

	var mu sync.Mutex
	var failures []string
	var readers sync.WaitGroup
	for i := 0; i < 2; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for n := 0; n < 500; n++ {
				if bad := validate(hub.Snapshot()); len(bad) > 0 {
					mu.Lock()
					failures = append(failures, bad...)
					mu.Unlock()
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	wg.Wait()

	assert.Empty(t, failures)
}
