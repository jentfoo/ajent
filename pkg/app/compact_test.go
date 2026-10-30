package app

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// textStream is one scripted provider turn emitting parts as a text message.
func textStream(parts ...string) []llm.Event {
	out := make([]llm.Event, 1, 1+len(parts)+2)
	out[0] = llm.Event{Type: llm.EventTextStart, Index: 0}
	var b strings.Builder
	for _, p := range parts {
		out = append(out, llm.Event{Type: llm.EventTextDelta, Index: 0, Text: p})
		b.WriteString(p)
	}
	out = append(out,
		llm.Event{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: b.String()}},
		llm.Event{Type: llm.EventDone, StopReason: llm.StopEndTurn})
	return out
}

// truncatedStream is one scripted provider turn emitting a single text message
// that stops at the output token cap: the final done event carries StopMaxTokens.
func truncatedStream(text string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: text},
		{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: text}},
		{Type: llm.EventDone, StopReason: llm.StopMaxTokens},
	}
}

// fullSummary is a complete six-section summariser reply, so scripted providers
// satisfy the section validation every recorded checkpoint must pass.
const fullSummary = `## Goal
user wanted a story about lighthouses

## Constraints & Preferences
- keep file paths verbatim

## Progress
### Done
- [x] chose the cut

### In Progress
- [ ] merging checkpoints

### Blocked
- (none)

## Key Decisions
- **band first**: newest steps stay verbatim

## Next Steps
1. resume the story

## Critical Context
- /w/story.txt holds the draft`

// testCompactor wires a compactor over a real transcript with a scripted provider.
func testCompactor(t *testing.T, model llm.Model, sp llm.Provider) (*compactor, *agent.State, *session.Writer) {
	t.Helper()

	store := session.StoreAt(t.TempDir())
	w, err := store.Create("/ws", session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
	st := &agent.State{Model: model, Tokens: tokens.New(model)}
	ag := agent.New(st, agent.Options{Sinks: []agent.Sink{agent.NopSink{}}})
	c := &compactor{
		rec: &sessRec{w: w}, st: st, ag: ag, reg: reg,
		sink:        agent.NopSink{},
		notify:      func(string, agent.Level) {},
		providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
		cfg:         func() config.Compaction { return config.Compaction{Auto: true} },
	}
	return c, st, w
}

// appendSteps records n assistant steps of roughly equal weight, so a session has
// something to fold once the newest steps are held back verbatim.
func appendSteps(t *testing.T, w *session.Writer, n int) session.Entry {
	t.Helper()

	var last session.Entry
	for i := 0; i < n; i++ {
		last = appendText(t, w, llm.RoleAssistant,
			"step "+strconv.Itoa(i)+" "+strings.Repeat("The Last Lighthouse. ", 200))
	}
	return last
}

func appendText(t *testing.T, w *session.Writer, role llm.Role, text string) session.Entry {
	t.Helper()

	e, err := w.Append(session.TypeMessage, session.MessageData{Message: llm.Text(role, text)})
	require.NoError(t, err)
	return e
}

// compactionEntries returns the compaction entries on the writer's live branch.
func compactionEntries(t *testing.T, w *session.Writer) []session.Entry {
	t.Helper()

	entries, _, err := session.Read(w.Path())
	require.NoError(t, err)
	var out []session.Entry
	for _, e := range session.Branch(entries, w.Head()) {
		if e.Type == session.TypeCompaction {
			out = append(out, e)
		}
	}
	return out
}

func TestCompactorManualSingleTurn(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textStream(fullSummary)},
	}}
	c, st, w := testCompactor(t, model, sp)

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)

	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.NoError(t, err)
	require.True(t, did)

	comps := compactionEntries(t, w)
	require.Len(t, comps, 1)
	var cd session.CompactionData
	require.NoError(t, comps[0].Decode(&cd))
	assert.NotEmpty(t, cd.FirstKeptEntryID)
	assert.Contains(t, cd.Summary, "lighthouse")
	assert.Less(t, cd.After, cd.Before)
	assert.Empty(t, cd.Reduce.Stubs)

	require.Len(t, st.Messages, 3) // the summary plus the two kept steps
	assert.Contains(t, textOfMain(st.Messages[0]), "lighthouse")
}

func TestCompactorReportsRebuiltContext(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textStream(fullSummary)},
	}}
	c, st, w := testCompactor(t, model, sp)

	var switched [][]llm.Message
	c.rec.onSwitch = func(msgs []llm.Message, cause string) { switched = append(switched, msgs) }

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)

	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.NoError(t, err)
	require.True(t, did)

	require.Len(t, switched, 1)
	assert.Equal(t, st.Messages, switched[0]) // the reduced context, not the old one
}

func TestCompactorPlansFromLiveHeadAfterRewind(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textStream(fullSummary)},
	}}
	c, st, w := testCompactor(t, model, sp)

	appendText(t, w, llm.RoleUser, "read me a short story")
	kept := appendSteps(t, w, 6)
	appendText(t, w, llm.RoleUser, "another")
	appendText(t, w, llm.RoleAssistant, strings.Repeat("Vending Machine. ", 200))
	w.SetHead(kept.ID) // rewind past the second turn, the old tail stays in the file

	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.NoError(t, err)
	require.True(t, did)

	require.NotEmpty(t, st.Messages)
	assert.Contains(t, textOfMain(st.Messages[0]), "story")
}

func TestCompactorOverflowRunsMidTurn(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 600, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Err: llm.ErrContextOverflow},     // oversized request
		{Events: textStream(fullSummary)}, // summariser
		{Events: textStream("recovered")}, // retried turn
	}}
	c, st, w := testCompactor(t, model, sp)

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)
	for _, e := range mustBranch(t, w) {
		var md session.MessageData
		if e.Decode(&md) == nil {
			st.Messages = append(st.Messages, md.Message)
		}
	}

	ag := agent.New(st, agent.Options{
		Sinks:    []agent.Sink{agent.NopSink{}},
		Provider: func(llm.Model) (llm.Provider, error) { return sp, nil },
		Env:      agent.DetectEnvironment(),
		Compact:  func(ctx context.Context, r agent.CompactReason) (bool, error) { return c.run(ctx, r, "") },
	})
	c.ag = ag

	err := ag.Prompt(t.Context(), agent.Input{Text: "once more"})
	require.NoError(t, err)

	require.NotEmpty(t, compactionEntries(t, w))
	var sb strings.Builder
	for _, m := range st.Messages {
		sb.WriteString(textOfMain(m))
	}
	assert.Contains(t, sb.String(), "recovered")
}

func TestCompactorAnnouncesStart(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 100}

	t.Run("announces_before_summarising", func(t *testing.T) {
		// several turns: the summariser prompt can chain across calls when the
		// span only fits by dropping its oldest entries
		sp := &llm.ScriptedProvider{Turns: func() []llm.ScriptedTurn {
			turns := make([]llm.ScriptedTurn, 4)
			for i := range turns {
				turns[i] = llm.ScriptedTurn{Events: textStream(fullSummary)}
			}
			return turns
		}()}
		c, st, w := testCompactor(t, model, sp)
		c.cfg = func() config.Compaction {
			return config.Compaction{Auto: true, MinSteps: 1, VerbatimFraction: 0.1}
		}
		var notices []string
		c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }
		appendText(t, w, llm.RoleUser, "read me a short story")
		appendSteps(t, w, 6)
		st.Tokens.Add(7000)

		did, err := c.run(t.Context(), agent.CompactStep, "")
		require.NoError(t, err)
		require.True(t, did)
		require.NotEmpty(t, notices)
		assert.Contains(t, notices[0], "compacting ")
	})

	t.Run("silent_when_it_declines_early", func(t *testing.T) {
		sp := &llm.ScriptedProvider{}
		c, st, w := testCompactor(t, model, sp)
		// a band wide enough to swallow the branch: chooseCut declines with no call
		c.cfg = func() config.Compaction {
			return config.Compaction{Auto: true, MinSteps: 8, VerbatimFraction: 0.9}
		}
		var notices []string
		c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }
		appendText(t, w, llm.RoleUser, "read me a short story")
		appendSteps(t, w, 2)
		st.Tokens.Add(7000)

		did, err := c.run(t.Context(), agent.CompactStep, "")
		require.NoError(t, err)
		require.False(t, did)
		assert.NotContains(t, strings.Join(notices, "\n"), "compacting ")
	})
}

func TestCompactorStallsWhenStillOverPoint(t *testing.T) {
	// a roomy window with a low compaction point, so the verbatim band alone
	// outweighs the point while the summariser prompt still fits
	model := llm.Model{Provider: "test", ID: "m",
		ContextWindow: 40000, MaxOutput: 1000, CompactThreshold: 3000}
	turns := make([]llm.ScriptedTurn, 4)
	for i := range turns {
		turns[i] = llm.ScriptedTurn{Events: textStream(fullSummary)}
	}
	c, st, w := testCompactor(t, model, &llm.ScriptedProvider{Turns: turns})
	c.cfg = func() config.Compaction {
		return config.Compaction{Auto: true, MinSteps: 6, VerbatimFraction: 0.9}
	}
	var notices []string
	c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }
	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 12)
	st.Tokens.Add(7000)

	did, err := c.run(t.Context(), agent.CompactStep, "")
	require.NoError(t, err)
	require.True(t, did)
	require.True(t, c.overPoint(model))
	require.True(t, c.stalled.Load())

	// the rest of the turn asks and is held, spending nothing
	summarised := strings.Count(strings.Join(notices, "\n"), "compacting ")
	for range 3 {
		did, err = c.run(t.Context(), agent.CompactStep, "")
		require.NoError(t, err)
		assert.False(t, did)
	}
	assert.Equal(t, summarised, strings.Count(strings.Join(notices, "\n"), "compacting "))

	// the turn boundary re-arms it: a fresh turn gets a fresh attempt
	_, err = c.run(t.Context(), agent.CompactThreshold, "")
	require.NoError(t, err)
	assert.False(t, c.stalled.Load())
}

func TestCompactorStepRunsMidTurn(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 100}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textStream(fullSummary)},  // summariser
		{Events: textStream("carried on")}, // the turn resumes after it
	}}
	c, st, w := testCompactor(t, model, sp)
	c.cfg = func() config.Compaction {
		return config.Compaction{Auto: true, MinSteps: 1, VerbatimFraction: 0.1}
	}
	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)
	st.Tokens.Add(7000) // past the compaction point for this window

	blocked := make(chan struct{})
	ag := agent.New(st, agent.Options{
		Sinks:    []agent.Sink{agent.NopSink{}},
		Provider: func(llm.Model) (llm.Provider, error) { return sp, nil },
		Env:      agent.DetectEnvironment(),
		Compact:  func(context.Context, agent.CompactReason) (bool, error) { <-blocked; return false, nil },
	})
	c.ag = ag

	errCh := make(chan error, 1)
	go func() { errCh <- ag.Prompt(t.Context(), agent.Input{Text: "x"}) }()
	require.Eventually(t, ag.Running, 5*time.Second, 10*time.Millisecond)

	did, err := c.run(t.Context(), agent.CompactStep, "")
	require.NoError(t, err)
	assert.True(t, did)
	assert.Len(t, compactionEntries(t, w), 1)

	close(blocked)
	require.NoError(t, <-errCh)
}

func TestCompactorReadFailureSurfaces(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}

	setup := func(t *testing.T) (*compactor, *[]string, *llm.ScriptedProvider) {
		t.Helper()

		sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textStream(fullSummary)}}}
		c, st, w := testCompactor(t, model, sp)
		c.cfg = func() config.Compaction { return config.Compaction{Auto: true} }
		appendText(t, w, llm.RoleUser, "read me a short story")
		appendSteps(t, w, 6)
		st.Tokens.Add(7000) // past the compaction point, so the automatic gates pass
		var notices []string
		c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }
		return c, &notices, sp
	}

	t.Run("manual_notifies", func(t *testing.T) {
		c, notices, sp := setup(t)
		require.NoError(t, os.Remove(c.rec.w.Path()))

		did, err := c.run(t.Context(), agent.CompactManual, "")
		require.Error(t, err)
		assert.False(t, did)
		assert.Contains(t, strings.Join(*notices, "\n"), "compaction failed: transcript unreadable")
		assert.Empty(t, sp.Requests()) // no summariser spend on a failed read
	})

	// the loop retries only a successful reduction, so the returned error is what
	// keeps the single overflow retry skipped while the failure stays visible
	t.Run("overflow_returns_error", func(t *testing.T) {
		c, notices, sp := setup(t)
		require.NoError(t, os.Remove(c.rec.w.Path()))

		did, err := c.run(t.Context(), agent.CompactOverflow, "")
		require.Error(t, err)
		assert.False(t, did)
		assert.Contains(t, strings.Join(*notices, "\n"), "compaction failed")
		assert.Empty(t, sp.Requests())
	})

	t.Run("step_notifies", func(t *testing.T) {
		c, notices, sp := setup(t)
		require.NoError(t, os.Remove(c.rec.w.Path()))

		did, err := c.run(t.Context(), agent.CompactStep, "")
		require.Error(t, err)
		assert.False(t, did)
		assert.Contains(t, strings.Join(*notices, "\n"), "compaction failed")
		assert.Empty(t, sp.Requests())
		// a failed read is not evidence this session cannot reduce
		assert.False(t, c.autoDisabled.Load())
	})
}

func TestCompactorNewerFormatRefused(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textStream(fullSummary)}}}
	c, _, w := testCompactor(t, model, sp)
	var notices []string
	c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)
	// a future build stamped this transcript, so Read reports it blocked
	_, err := w.Append(session.TypeSession, session.SessionData{Version: session.Version() + 1})
	require.NoError(t, err)
	head := w.Head()

	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.ErrorIs(t, err, session.ErrNewerFormat)
	assert.False(t, did)
	assert.Contains(t, strings.Join(notices, "\n"), "compaction failed")
	assert.Equal(t, head, w.Head()) // a blocked transcript gains no entries
	assert.Empty(t, sp.Requests())
}

func TestCompactorEmptyTranscriptReportsNothing(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{}
	c, st, w := testCompactor(t, model, sp)
	c.cfg = func() config.Compaction { return config.Compaction{Auto: true} }
	var notices []string
	c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }

	appendText(t, w, llm.RoleUser, "read me a short story")
	st.Tokens.Add(7000)                          // past the compaction point, so the step trigger reaches the read
	require.NoError(t, os.Truncate(w.Path(), 0)) // readable but holds no lines

	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.NoError(t, err)
	assert.False(t, did)
	assert.Contains(t, strings.Join(notices, "\n"), "nothing to compact")

	notices = nil
	did, err = c.run(t.Context(), agent.CompactStep, "")
	require.NoError(t, err)
	assert.False(t, did)
	assert.Empty(t, notices) // a thin session is not a failure and never latches
	assert.False(t, c.autoDisabled.Load())
}

func TestCompactorRunRefusesInFlight(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 100}

	setup := func(t *testing.T) (*compactor, *[]string, *session.Writer) {
		t.Helper()

		sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textStream(fullSummary)}}}
		c, st, w := testCompactor(t, model, sp)
		appendText(t, w, llm.RoleUser, "read me a short story")
		appendSteps(t, w, 6)
		st.Tokens.Add(7000)
		var notices []string
		c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }
		return c, &notices, w
	}

	t.Run("manual_notifies", func(t *testing.T) {
		c, notices, w := setup(t)
		c.inFlight.Store(true) // a drain-goroutine fold is in the boundary gap

		did, err := c.run(t.Context(), agent.CompactManual, "")
		require.NoError(t, err)
		assert.False(t, did)
		assert.Contains(t, strings.Join(*notices, "\n"),
			"compaction refused: a compaction is already running")
		assert.Empty(t, compactionEntries(t, w))
	})

	t.Run("auto_is_silent", func(t *testing.T) {
		c, notices, w := setup(t)
		c.cfg = func() config.Compaction { return config.Compaction{Auto: true} }
		c.inFlight.Store(true)

		did, err := c.run(t.Context(), agent.CompactThreshold, "")
		require.NoError(t, err)
		assert.False(t, did)
		assert.Empty(t, *notices)
		assert.Empty(t, compactionEntries(t, w))
	})

	t.Run("runs_again_once_clear", func(t *testing.T) {
		c, _, w := setup(t)
		c.inFlight.Store(true)
		_, _ = c.run(t.Context(), agent.CompactManual, "") // refused, the owner still holds it
		c.inFlight.Store(false)                            // its run returned

		did, err := c.run(t.Context(), agent.CompactManual, "")
		require.NoError(t, err)
		assert.True(t, did)
		assert.Len(t, compactionEntries(t, w), 1)
	})
}

// gatedProvider holds Stream until released, so a test can start a turn while a
// summariser call is in flight.
type gatedProvider struct {
	started chan struct{}
	release chan struct{}
	events  []llm.Event
}

func (p *gatedProvider) Name() string { return "gated" }

func (p *gatedProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	p.started <- struct{}{}
	<-p.release
	return &llm.SliceStream{Events: p.events}, nil
}

func TestCompactorSkipsReseedWhenTurnStarts(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	gate := &gatedProvider{
		started: make(chan struct{}, 4), // a chained summary call buffers harmlessly
		release: make(chan struct{}),
		events:  textStream(fullSummary),
	}
	c, st, w := testCompactor(t, model, gate)

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)
	for _, e := range mustBranch(t, w) {
		var md session.MessageData
		if e.Decode(&md) == nil {
			st.Messages = append(st.Messages, md.Message)
		}
	}

	sink := &ctxSink{}
	c.sink = sink
	var notices []string
	c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }

	blocked := &blockingProvider{
		turn:     []llm.Event{{Type: llm.EventTextDelta, Text: "x"}},
		onCreate: make(chan *blockingStream, 1),
	}
	ag := agent.New(st, agent.Options{
		Sinks:    []agent.Sink{agent.NopSink{}},
		Provider: func(llm.Model) (llm.Provider, error) { return blocked, nil },
		Env:      agent.DetectEnvironment(),
	})
	c.ag = ag

	type res struct {
		did bool
		err error
	}
	compCh := make(chan res, 1)
	go func() {
		did, err := c.run(t.Context(), agent.CompactManual, "")
		compCh <- res{did, err}
	}()
	<-gate.started // the summariser call is in flight, past the Running() guard

	turnCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	turnCh := make(chan error, 1)
	go func() { turnCh <- ag.Prompt(turnCtx, agent.Input{Text: "meanwhile"}) }()
	select {
	case <-blocked.onCreate: // prompt landed, the turn is blocked mid-stream
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never started")
	}
	before := len(st.Messages)
	used := st.Tokens.Context().Used

	close(gate.release) // the summary lands, the swap now races a live turn

	var got res
	select {
	case got = <-compCh:
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not return")
	}
	require.NoError(t, got.err)
	assert.False(t, got.did)                        // nothing installed live
	assert.Len(t, compactionEntries(t, w), 1)       // the entry is recorded
	assert.Len(t, st.Messages, before)              // the reduced list never landed
	assert.Equal(t, used, st.Tokens.Context().Used) // no reseed
	assert.Zero(t, sink.last.Used)                  // no bar push
	assert.False(t, c.inFlight.Load())              // the flag released
	assert.Contains(t, strings.Join(notices, "\n"), "compaction recorded but the state rebuild failed")

	cancel() // release the blocked turn
	require.NoError(t, <-turnCh)
}

func TestCompactorDeclineLatchesAuto(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 100}

	// the summariser returns a checkpoint bigger than the span it replaces, so the
	// fold is really attempted and finish rejects it for saving nothing
	setup := func(t *testing.T) (*compactor, *[]string) {
		t.Helper()

		// huge but complete: validation must not spend a scripted retry here
		bloated := strings.Repeat("the lighthouse keeper wrote it all down. ", 1000) + "\n" + fullSummary
		turns := make([]llm.ScriptedTurn, 4)
		for i := range turns {
			turns[i] = llm.ScriptedTurn{Events: textStream(bloated)}
		}
		c, st, w := testCompactor(t, model, &llm.ScriptedProvider{Turns: turns})
		c.cfg = func() config.Compaction {
			return config.Compaction{Auto: true, MinSteps: 1, VerbatimFraction: 0.1}
		}
		var notices []string
		c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }
		appendText(t, w, llm.RoleUser, "read me a short story")
		appendSteps(t, w, 6)
		st.Tokens.Add(7000) // past the compaction point
		return c, &notices
	}

	// a session with nothing worth folding yet is not a session that cannot reduce:
	// latching there would disable automatic compaction as history was still arriving.
	t.Run("nothing_to_fold_yet_never_latches", func(t *testing.T) {
		// several turns: the summariser prompt can chain across calls when the
		// span only fits by dropping its oldest entries
		sp := &llm.ScriptedProvider{Turns: func() []llm.ScriptedTurn {
			turns := make([]llm.ScriptedTurn, 4)
			for i := range turns {
				turns[i] = llm.ScriptedTurn{Events: textStream(fullSummary)}
			}
			return turns
		}()}
		c, st, w := testCompactor(t, model, sp)
		c.cfg = func() config.Compaction {
			return config.Compaction{Auto: true, MinSteps: 8, VerbatimFraction: 0.9}
		}
		var notices []string
		c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }
		appendText(t, w, llm.RoleUser, "read me a short story")
		appendSteps(t, w, 2) // the band swallows the branch, chooseCut declines for free
		st.Tokens.Add(7000)

		did, err := c.run(t.Context(), agent.CompactStep, "")
		require.NoError(t, err)
		require.False(t, did)
		require.False(t, c.autoDisabled.Load())
		assert.NotContains(t, strings.Join(notices, "\n"), "could not reduce")

		// once enough history has accrued the very next step boundary compacts
		c.cfg = func() config.Compaction {
			return config.Compaction{Auto: true, MinSteps: 1, VerbatimFraction: 0.1}
		}
		appendSteps(t, w, 6)
		did, err = c.run(t.Context(), agent.CompactStep, "")
		require.NoError(t, err)
		assert.True(t, did)
	})

	t.Run("second_attempt_is_skipped", func(t *testing.T) {
		c, notices := setup(t)

		did, err := c.run(t.Context(), agent.CompactStep, "")
		require.NoError(t, err)
		require.False(t, did)
		require.True(t, c.autoDisabled.Load())
		assert.Contains(t, strings.Join(*notices, "\n"), "could not reduce this session")

		// the latch short-circuits before the transcript is even read
		c.rec = nil // a re-attempt would nil-panic on rec.w
		did, err = c.run(t.Context(), agent.CompactStep, "")
		require.NoError(t, err)
		assert.False(t, did)
	})

	t.Run("one_reminder_per_turn", func(t *testing.T) {
		c, notices := setup(t)

		require.NoError(t, warmLatch(t, c))
		*notices = nil
		c.rec = nil // prove nothing re-attempts

		for range 3 { // three step boundaries inside one turn
			_, err := c.run(t.Context(), agent.CompactStep, "")
			require.NoError(t, err)
		}
		assert.Empty(t, *notices)

		_, err := c.run(t.Context(), agent.CompactThreshold, "") // turn boundary re-arms it
		require.NoError(t, err)
		for range 3 {
			_, err := c.run(t.Context(), agent.CompactStep, "")
			require.NoError(t, err)
		}
		require.Len(t, *notices, 1)
		assert.Contains(t, (*notices)[0], "over the compaction point")
	})

	// below the bar run never reaches pkg/compact, so it is not a decline: latching
	// there would disable automatic compaction on a session that never tried.
	t.Run("below_threshold_never_latches", func(t *testing.T) {
		sp := &llm.ScriptedProvider{}
		c, _, w := testCompactor(t, model, sp)
		c.cfg = func() config.Compaction {
			return config.Compaction{Auto: true, MinSteps: 8, VerbatimFraction: 0.9}
		}
		appendText(t, w, llm.RoleUser, "read me a short story")
		appendSteps(t, w, 2) // no Tokens.Add: still under the compaction point

		did, err := c.run(t.Context(), agent.CompactStep, "")
		require.NoError(t, err)
		assert.False(t, did)
		assert.False(t, c.autoDisabled.Load())
	})

	t.Run("manual_never_latches", func(t *testing.T) {
		c, _ := setup(t)

		did, err := c.run(t.Context(), agent.CompactManual, "")
		require.NoError(t, err)
		require.False(t, did)
		assert.False(t, c.autoDisabled.Load())
	})

	t.Run("success_clears_the_latch", func(t *testing.T) {
		// several turns: the summariser prompt can chain across calls when the
		// span only fits by dropping its oldest entries
		sp := &llm.ScriptedProvider{Turns: func() []llm.ScriptedTurn {
			turns := make([]llm.ScriptedTurn, 4)
			for i := range turns {
				turns[i] = llm.ScriptedTurn{Events: textStream(fullSummary)}
			}
			return turns
		}()}
		c, st, w := testCompactor(t, model, sp)
		c.cfg = func() config.Compaction {
			return config.Compaction{Auto: true, MinSteps: 1, VerbatimFraction: 0.1}
		}
		appendText(t, w, llm.RoleUser, "read me a short story")
		appendSteps(t, w, 6)
		st.Tokens.Add(7000)
		c.autoDisabled.Store(true)
		c.warned.Store(true)

		did, err := c.run(t.Context(), agent.CompactManual, "")
		require.NoError(t, err)
		require.True(t, did)
		assert.False(t, c.autoDisabled.Load(), "this session still reduces")
	})

	t.Run("resume_auto_re_arms", func(t *testing.T) {
		c, _ := setup(t)
		c.autoDisabled.Store(true)
		c.warned.Store(true)

		c.resumeAuto()
		assert.False(t, c.autoDisabled.Load())
		assert.False(t, c.warned.Load())
	})
}

// warmLatch runs one declining automatic compaction so the latch is set.
func warmLatch(t *testing.T, c *compactor) error {
	t.Helper()

	did, err := c.run(t.Context(), agent.CompactStep, "")
	require.False(t, did)
	require.True(t, c.autoDisabled.Load())
	return err
}

func mustBranch(t *testing.T, w *session.Writer) []session.Entry {
	t.Helper()

	entries, _, err := session.Read(w.Path())
	require.NoError(t, err)
	return session.Branch(entries, w.Head())
}

// textOfMain returns the concatenated text blocks of a message.
func textOfMain(m llm.Message) string {
	var sb strings.Builder
	for _, b := range m.Content {
		if tb, ok := b.(llm.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// ctxSink captures the last Context event a compactor pushes.
type ctxSink struct{ last tokens.ContextState }

func (c *ctxSink) Context(s tokens.ContextState) { c.last = s }
func (c *ctxSink) TurnStart(agent.TurnInfo)      {}
func (c *ctxSink) UserPrompt(string)             {}
func (c *ctxSink) InjectContext(string)          {}
func (c *ctxSink) Thinking(string)               {}
func (c *ctxSink) EndThinking()                  {}
func (c *ctxSink) Text(string)                   {}
func (c *ctxSink) EndText()                      {}
func (c *ctxSink) ToolStart(agent.ToolCall, string, bool) func(agent.ToolResult) {
	return func(agent.ToolResult) {}
}
func (c *ctxSink) ToolOutput(string, string)       {}
func (c *ctxSink) ToolProgress(agent.ToolProgress) {}
func (c *ctxSink) Diff(string, string, string)     {}
func (c *ctxSink) Usage(llm.Usage)                 {}
func (c *ctxSink) Notice(string, agent.Level)      {}
func (c *ctxSink) TurnEnd(agent.TurnResult)        {}

func TestCompactorReseedReflectsReducedFullUsage(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	// several turns: the summariser prompt can chain across calls when the span
	// only fits by dropping its oldest entries
	turns := make([]llm.ScriptedTurn, 4)
	for i := range turns {
		turns[i] = llm.ScriptedTurn{Events: textStream(fullSummary)}
	}
	sp := &llm.ScriptedProvider{Turns: turns}
	c, st, w := testCompactor(t, model, sp)

	msgs := []llm.Message{llm.Text(llm.RoleUser, "read me a short story")}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, llm.Text(llm.RoleAssistant,
			"step "+strconv.Itoa(i)+" "+strings.Repeat("The Last Lighthouse. ", 500)))
	}
	for _, m := range msgs {
		st.Messages = append(st.Messages, m)
		_, err := w.Append(session.TypeMessage, session.MessageData{Message: m})
		require.NoError(t, err)
	}
	// a settled calibrator overestimates raw estimates and a large ledger base sits
	// on top. Both must apply to the reseed exactly as they do to every estimate.
	predicted := tokens.EstimateMessages(msgs)
	st.Tokens.SetBase(9000)
	st.Tokens.Response("test/m", llm.Usage{Input: predicted * 4, Output: 100}, predicted*2, true)

	sink := &ctxSink{}
	c.sink = sink
	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.NoError(t, err)
	require.True(t, did)

	comp := compactionEntries(t, w)
	require.Len(t, comp, 1)
	var cd session.CompactionData
	require.NoError(t, comp[0].Decode(&cd))
	// recorded Before counts full usage: fixed overhead plus all messages
	base := c.ag.BaseEstimate(true)
	assert.Equal(t, base+tokens.EstimateMessages(msgs), cd.Before)
	assert.Less(t, cd.After, cd.Before)

	// the bar drops below the stale exact usage immediately and stays an estimate:
	// pending holds the reduced messages, the ledger base adds once, factor scales.
	cs := st.Tokens.Context()
	assert.Less(t, cs.Used, predicted*4)
	assert.True(t, cs.Estimated)
	assert.Equal(t, 2*(cd.After-base+9000), cs.Used)
	assert.Equal(t, sink.last.Used, cs.Used)
}

// blockingStream delivers one text event then blocks in Next until Close, so a
// caller can cancel mid-drain. It mirrors pkg/agent's hangStream for package main.
type blockingStream struct {
	events []llm.Event

	mu      sync.Mutex
	pos     int
	done    chan struct{}
	closed  bool            // set once Close runs, so a repeat Close stays silent
	onClose chan<- struct{} // signalled (once) when the stream is closed
}

func (s *blockingStream) Next() (llm.Event, bool) {
	s.mu.Lock()
	if !s.closed && s.pos < len(s.events) {
		i := s.pos
		s.pos++
		ev := s.events[i]
		s.mu.Unlock()
		return ev, true // deliver events immediately, hold the pull open after them
	}
	// no more events and not closed: block until Close abandons the stream
	done := s.done
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		<-done
	}
	return llm.Event{}, false
}

func (s *blockingStream) Err() error { return nil }

// Close unblocks a pending Next and signals the owner once.
func (s *blockingStream) Close() error {
	s.mu.Lock()
	if s.done != nil {
		close(s.done)
		s.done = nil
	}
	first := !s.closed
	s.closed = true
	onClose := s.onClose
	s.mu.Unlock()
	if first && onClose != nil {
		select {
		case onClose <- struct{}{}:
		default:
		}
	}
	return nil
}

// blockingProvider serves one turn through a blockingStream. onCreate reports the
// stream to its owner, and onClosed is signalled when the stream closes.
type blockingProvider struct {
	turn     []llm.Event
	onCreate chan *blockingStream
	onClose  chan struct{}
	created  atomic.Int32 // streams created so far, for re-invocation assertions
}

func (p *blockingProvider) Name() string { return "block" }

func (p *blockingProvider) Stream(_ context.Context, _ llm.Request) (llm.Stream, error) {
	p.created.Add(1)
	s := &blockingStream{events: p.turn, done: make(chan struct{}), onClose: p.onClose}
	if p.onCreate != nil {
		p.onCreate <- s
	}
	return s, nil
}

func TestRunSummaryCancelStopsDraining(t *testing.T) {
	t.Parallel()

	onClose := make(chan struct{}, 1)
	p := &blockingProvider{
		turn:    []llm.Event{{Type: llm.EventTextDelta, Text: "read"}}, // prefix-matches readonly
		onClose: onClose,
	}
	ctx, cancel := context.WithCancel(t.Context())

	type res struct {
		text string
		err  error
	}
	resCh := make(chan res, 1)
	go func() {
		text, _, err := llm.RunSummary(ctx, p, llm.Request{})
		resCh <- res{text, err}
	}()

	cancel()

	var got res
	select {
	case got = <-resCh:
	case <-time.After(time.Second * 3):
		t.Fatal("runSummary did not return after cancellation")
	}
	require.ErrorIs(t, got.err, context.Canceled)
	assert.Empty(t, got.text) // never partial text: "read" must not normalize to readonly

	// the stream was closed so its blocked Next unblocked
	require.Eventually(t, func() bool {
		select {
		case <-onClose:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestRunSummaryRejectsTruncatedResponses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		turn []llm.Event
	}{
		{"max_tokens_stop_is_an_error", truncatedStream("## Goal\npar")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: tc.turn}}}
			text, usage, err := llm.RunSummary(t.Context(), sp, llm.Request{})
			require.ErrorIs(t, err, llm.ErrTruncated)
			assert.Empty(t, text)
			assert.Zero(t, usage.Input+usage.Output)
		})
	}

	t.Run("end_turn_stop_returns_text", func(t *testing.T) {
		sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textStream("## Goal\nfull")}}}
		text, _, err := llm.RunSummary(t.Context(), sp, llm.Request{})
		require.NoError(t, err)
		assert.Equal(t, "## Goal\nfull", text)
	})
}

func TestCompactorRejectsTruncatedSummary(t *testing.T) {
	t.Parallel()

	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 100}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: truncatedStream("## Goal\npartial sum")},
	}}
	c, st, w := testCompactor(t, model, sp)
	c.cfg = func() config.Compaction { return config.Compaction{Auto: true, MinSteps: 1, VerbatimFraction: 0.1} }
	var notices []string
	c.notify = func(msg string, level agent.Level) { notices = append(notices, msg) }

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)
	before := len(st.Messages)

	did, err := c.run(t.Context(), agent.CompactManual, "")
	assert.False(t, did)
	require.ErrorIs(t, err, llm.ErrTruncated)
	require.Empty(t, compactionEntries(t, w))
	joined := strings.Join(notices, "\n")
	assert.Contains(t, joined, "compaction failed")
	assert.Len(t, st.Messages, before) // state untouched
}

func TestCompactorSpendsFailedSummaryCall(t *testing.T) {
	t.Parallel()

	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 100}
	// a usage report rides in before the drain rejects the truncated reply:
	// the provider bills those tokens whether or not a checkpoint lands
	billed := llm.Usage{Input: 5000, Output: 20}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: append([]llm.Event{{Type: llm.EventUsage, Usage: billed}},
			truncatedStream("## Goal\npartial sum")...)},
	}}
	c, st, w := testCompactor(t, model, sp)

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)

	bar := st.Tokens.Context().Used
	did, err := c.run(t.Context(), agent.CompactManual, "")
	assert.False(t, did)
	require.ErrorIs(t, err, llm.ErrTruncated)

	total := st.Tokens.Total()
	assert.Equal(t, billed.Input, total.Input)
	assert.Equal(t, billed.Output, total.Output)
	// spend-only: a failed compaction never moves the context bar
	assert.Equal(t, bar, st.Tokens.Context().Used)
	assert.Empty(t, compactionEntries(t, w))
}

func TestCompactorWarnsOnSectionlessSummary(t *testing.T) {
	t.Parallel()

	// both attempts lack the six required headings: one bounded retry, then the
	// fold records anyway and says so where every other compaction warning lands.
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 100}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textStream("a freeform reply without the required headings")},
		{Events: textStream("still freeform on the retry")},
	}}
	c, _, w := testCompactor(t, model, sp)
	var notices []string
	c.notify = func(msg string, level agent.Level) { notices = append(notices, msg) }

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)

	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.NoError(t, err)
	require.True(t, did) // the fold itself is not failed by a defective summary
	assert.Len(t, compactionEntries(t, w), 1)
	joined := strings.Join(notices, "\n")
	assert.Contains(t, joined, "compact: summary is missing required sections")
}

func TestCompactorAutoSetting(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 100}

	setup := func(t *testing.T, auto bool) (*compactor, *session.Writer) {
		t.Helper()

		// several turns: the summariser prompt can chain across calls when the
		// span only fits by dropping its oldest entries
		sp := &llm.ScriptedProvider{Turns: func() []llm.ScriptedTurn {
			turns := make([]llm.ScriptedTurn, 4)
			for i := range turns {
				turns[i] = llm.ScriptedTurn{Events: textStream(fullSummary)}
			}
			return turns
		}()}
		c, st, w := testCompactor(t, model, sp)
		// a tail of a tenth of the compaction point, so a non-forced run has an
		// older turn to fold rather than declining
		c.cfg = func() config.Compaction {
			return config.Compaction{Auto: auto, MinSteps: 1, VerbatimFraction: 0.1}
		}

		appendText(t, w, llm.RoleUser, "read me a short story")
		appendSteps(t, w, 6)
		st.Tokens.Add(7000) // past the compaction point for this window
		return c, w
	}

	t.Run("threshold_refused_when_off", func(t *testing.T) {
		c, w := setup(t, false)

		did, err := c.run(t.Context(), agent.CompactThreshold, "")
		require.NoError(t, err)
		assert.False(t, did)
		assert.Empty(t, compactionEntries(t, w))
	})

	t.Run("threshold_runs_when_on", func(t *testing.T) {
		c, w := setup(t, true)

		did, err := c.run(t.Context(), agent.CompactThreshold, "")
		require.NoError(t, err)
		require.True(t, did)
		assert.Len(t, compactionEntries(t, w), 1)
	})

	t.Run("manual_ignores_auto_off", func(t *testing.T) {
		c, w := setup(t, false)

		did, err := c.run(t.Context(), agent.CompactManual, "")
		require.NoError(t, err)
		require.True(t, did)
		assert.Len(t, compactionEntries(t, w), 1)
	})

	t.Run("overflow_ignores_auto_off", func(t *testing.T) {
		c, w := setup(t, false)

		did, err := c.run(t.Context(), agent.CompactOverflow, "")
		require.NoError(t, err)
		require.True(t, did)
		assert.Len(t, compactionEntries(t, w), 1)
	})
}

func TestCompactorProviderError(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}

	sp := &llm.ScriptedProvider{}
	c, _, w := testCompactor(t, model, sp)
	var notices []string
	c.notify = func(msg string, level agent.Level) { notices = append(notices, msg) }
	// the summariser provider is gone: run must fail loudly.
	c.providerFor = func(llm.Model) (llm.Provider, error) {
		return nil, errors.New("no such model")
	}

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)

	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.Error(t, err)
	assert.False(t, did)
	joined := strings.Join(notices, "\n")
	assert.Contains(t, joined, "unavailable", "a setup failure is not a successful no-op")
	assert.NotContains(t, joined, "nothing to compact")
	// nothing was appended for the failed run.
	require.Empty(t, compactionEntries(t, w))
}

func TestEndTurnClearsStalled(t *testing.T) {
	// a roomy window with a low compaction point, so the verbatim band alone
	// outweighs the point while there is still history to fold
	model := llm.Model{Provider: "test", ID: "m",
		ContextWindow: 40000, MaxOutput: 1000, CompactThreshold: 3000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textStream(fullSummary)},
	}}
	c, st, w := testCompactor(t, model, sp)
	var notices []string
	c.notify = func(msg string, _ agent.Level) { notices = append(notices, msg) }
	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 12)
	st.Tokens.Add(7000)

	// the mid-turn compact left the point crossed and stalled armed, so the next step
	// in that same turn is held off without attempting a fold.
	c.stalled.Store(true)
	did, err := c.run(t.Context(), agent.CompactStep, "")
	require.NoError(t, err)
	assert.False(t, did)
	require.Empty(t, compactionEntries(t, w))

	// the errored turn's boundary still re-arms per-turn state
	c.endTurn()
	assert.False(t, c.stalled.Load())
	assert.False(t, c.warned.Load())

	// the next turn's first step can fold again rather than being held off
	did, err = c.run(t.Context(), agent.CompactStep, "")
	require.NoError(t, err)
	require.True(t, did)
}
