package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// testCompactor wires a compactor over a real transcript with a scripted provider.
func testCompactor(t *testing.T, model llm.Model, sp *llm.ScriptedProvider) (*compactor, *agent.State, *session.Writer) {
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

// A manual compact on a single-turn session folds the whole history into a
// a real cut, not a summary-only one: the newest steps stay verbatim.
func TestCompactorManualSingleTurn(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textStream("## Goal\nuser wanted a story about a lighthouse")},
	}}
	c, st, w := testCompactor(t, model, sp)

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)

	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.NoError(t, err)
	require.True(t, did, "a session with foldable steps must compact")

	comps := compactionEntries(t, w)
	require.Len(t, comps, 1)
	var cd session.CompactionData
	require.NoError(t, comps[0].Decode(&cd))
	assert.NotEmpty(t, cd.FirstKeptEntryID, "the newest steps stay verbatim")
	assert.Contains(t, cd.Summary, "lighthouse")
	assert.Less(t, cd.After, cd.Before)
	assert.Empty(t, cd.Reduce.Stubs, "the cut already removed everything a stub could touch")

	require.Len(t, st.Messages, 3) // the summary plus the two kept steps
	assert.Contains(t, textOfMain(st.Messages[0]), "lighthouse")
}

// A compaction cuts and elides tool results, taking file content out of context
// that read tracking still vouches for, so it must report the rebuild like a
// rewind does — or a later @ref would dedupe against content the model lost.
func TestCompactorReportsRebuiltContext(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textStream("## Goal\nuser wanted a story")},
	}}
	c, st, w := testCompactor(t, model, sp)

	var switched [][]llm.Message
	c.rec.onSwitch = func(msgs []llm.Message) { switched = append(switched, msgs) }

	appendText(t, w, llm.RoleUser, "read me a short story")
	appendSteps(t, w, 6)

	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.NoError(t, err)
	require.True(t, did)

	require.Len(t, switched, 1)
	assert.Equal(t, st.Messages, switched[0]) // the reduced context, not the old one
}

// After a rewind the file tail sits on an abandoned branch; planning must use
// the writer's live head or the recorded cut cannot be found on rebuild.
func TestCompactorPlansFromLiveHeadAfterRewind(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textStream("## Goal\nuser wanted a story")},
	}}
	c, st, w := testCompactor(t, model, sp)

	appendText(t, w, llm.RoleUser, "read me a short story")
	kept := appendSteps(t, w, 6)
	appendText(t, w, llm.RoleUser, "another")
	appendText(t, w, llm.RoleAssistant, strings.Repeat("Vending Machine. ", 200))
	w.SetHead(kept.ID) // rewind past the second turn; the old tail stays in the file

	did, err := c.run(t.Context(), agent.CompactManual, "")
	require.NoError(t, err)
	require.True(t, did)

	require.NotEmpty(t, st.Messages, "a cut that cannot be located must not wipe context")
	assert.Contains(t, textOfMain(st.Messages[0]), "story")
}

// An overflow compaction fires mid-turn from the turn's own goroutine; it must
// not be refused for running, and the retried request must see the reduced context.
func TestCompactorOverflowRunsMidTurn(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 600, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Err: llm.ErrContextOverflow},                        // oversized request
		{Events: textStream("## Goal\nuser wanted stories")}, // summariser
		{Events: textStream("recovered")},                    // retried turn
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
func (c *ctxSink) Thinking(string)               {}
func (c *ctxSink) EndThinking()                  {}
func (c *ctxSink) Text(string)                   {}
func (c *ctxSink) EndText()                      {}
func (c *ctxSink) ToolStart(agent.ToolCall, string) func(agent.ToolResult) {
	return func(agent.ToolResult) {}
}
func (c *ctxSink) ToolOutput(string, string)       {}
func (c *ctxSink) ToolProgress(agent.ToolProgress) {}
func (c *ctxSink) Diff(string, string, string)     {}
func (c *ctxSink) Usage(llm.Usage)                 {}
func (c *ctxSink) Notice(string, agent.Level)      {}
func (c *ctxSink) TurnEnd(agent.TurnResult)        {}

// After a manual /compact the bar must drop to a calibrated estimate of the
// reduced full usage: pending carries the reduced messages, the ledger's base
// rides on top once, and the calibrator's factor still applies (the ~ marker
// stays). The recorded Before/After include base, and the summariser's own call
// must never snap the context terms.
func TestCompactorReseedReflectsReducedFullUsage(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 1000}
	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textStream("## Goal\nuser wanted a story about lighthouses")},
	}}
	c, st, w := testCompactor(t, model, sp)

	msgs := []llm.Message{llm.Text(llm.RoleUser, "read me a short story")}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, llm.Text(llm.RoleAssistant,
			"step "+strconv.Itoa(i)+" "+strings.Repeat("The Last Lighthouse. ", 500)))
	}
	for _, m := range msgs {
		st.Messages = append(st.Messages, m)
		if _, err := w.Append(session.TypeMessage, session.MessageData{Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	// a settled calibrator overestimates raw estimates and a large ledger base sits
	// on top; both must apply to the reseed exactly as they do to every estimate.
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
	// recorded Before counts full usage: fixed overhead plus all messages.
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
		return ev, true // deliver events immediately; hold the pull open after them
	}
	// no more events and not closed: block until Close abandons the stream.
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
// stream to its owner; onClosed is signalled when the stream closes.
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

// TestRunSummaryCancelStopsDraining cancels mid-drain and asserts runSummary stops
// promptly with context.Canceled instead of returning partial text.
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
		text, _, err := runSummary(ctx, p, llm.Request{})
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

// The compaction.auto setting gates only the threshold trigger: a user turning
// automatic reduction off must still be able to run /compact, and an overflow
// must still recover rather than brick the session.
func TestCompactorAutoSetting(t *testing.T) {
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 8000, MaxOutput: 100}

	setup := func(t *testing.T, auto bool) (*compactor, *session.Writer) {
		t.Helper()
		sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: textStream("## Goal\nthe lighthouse story")},
		}}
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

// A compaction whose summariser provider cannot be resolved must report the
// setup failure rather than masquerade as a successful "nothing to compact".
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

func TestVerbatimTokens(t *testing.T) {
	t.Parallel()

	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000} // compacts at 160k

	cases := []struct {
		name     string
		fraction float64
		want     int
	}{
		{"tenth_of_the_compaction_point", 0.1, 16000},
		{"zero_defers_to_compact", 0, 0},
		{"negative_defers_to_compact", -0.5, 0},
		{"whole_defers_to_compact", 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, verbatimTokens(model, tc.fraction))
		})
	}
}
