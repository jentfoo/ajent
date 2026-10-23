package agent

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
)

func TestSteerIdleRejected(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}
	a := newTestAgent(nil, p, nil)

	assert.False(t, a.Steer(Input{Text: "x"}))
	assert.False(t, a.FollowUp(Input{Text: "x"}))
}

func TestRunningReportsState(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	assert.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval)

	close(block) // let it finish
	require.NoError(t, <-errCh)
	assert.False(t, a.Running())
}

func TestSteerDeliveredFiresWhenLanded(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	var delivered int
	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	assert.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval)

	require.True(t, a.Steer(Input{Text: "nudge", Delivered: func() { delivered++ }}))
	close(block) // let the tool finish so the steer lands at the next step boundary
	require.NoError(t, <-errCh)
	assert.Equal(t, 1, delivered)
}

func TestOnBoundaryPullsAtStepBoundary(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	// the callback is armed mid-turn (once bash blocks) so its input lands at the
	// step boundary after the tool result rather than riding turn one. The mutex
	// keeps arming race-free against the loop goroutine reading it.
	var mu sync.Mutex
	pending := []Input{}
	a.opts.OnBoundary = func() []Input {
		mu.Lock()
		defer mu.Unlock()
		if len(pending) == 0 {
			return nil
		}
		out := pending
		pending = nil
		return out
	}

	var delivered int
	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	assert.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval)

	// wait until bash is executing (blocked), then arm and release it
	tool := set.tools["bash"].(*stubTool)
	assert.Eventually(t, func() bool { return tool.callCount() >= 1 }, defaultTimeout, pollInterval)
	mu.Lock()
	pending = []Input{{Text: "nudge", Delivered: func() { delivered++ }}}
	mu.Unlock()

	close(block)
	require.NoError(t, <-errCh)
	assert.Equal(t, 1, delivered)

	reqs := p.Requests()
	require.Len(t, reqs, 2)
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	require.Equal(t, llm.RoleUser, last.Role)
	tb, ok := last.Content[0].(llm.TextBlock)
	require.True(t, ok)
	assert.Equal(t, "nudge", tb.Text)
}

func TestOnBoundaryEmptyIsNoop(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}
	a := newTestAgent(nil, p, nil)
	var called int
	a.opts.OnBoundary = func() []Input {
		called++
		return nil
	}

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	require.NoError(t, <-errCh)
	assert.Equal(t, 1, called) // a single-step turn hits exactly one boundary
}

func TestSteerEmptyNeverDelivered(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	var delivered int
	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	assert.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval)

	require.True(t, a.Steer(Input{Delivered: func() { delivered++ }}))
	close(block)
	require.NoError(t, <-errCh)
	assert.Equal(t, 0, delivered) // nothing landed, so no confirmation
}

func TestPromptBehindDriverQueues(t *testing.T) {
	t.Parallel()

	// a mid-turn Prompt must queue behind the active driver and return at once;
	// waiting on the drain would deadlock a callback running on the loop goroutine
	block := make(chan struct{})
	tool := &stubTool{name: "bash", result: "ok", block: block}
	set := &mapSet{tools: map[string]Tool{"bash": tool}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")}, // turn one, blocked on the tool
		{Events: textOnly("one done")},
		{Events: textOnly("two done")}, // the queued prompt's own reply
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "one"}) }()
	require.Eventually(t, func() bool {
		return a.Running() && tool.callCount() > 0
	}, defaultTimeout, pollInterval)

	handoff := make(chan error, 1)
	go func() { handoff <- a.Prompt(t.Context(), Input{Text: "two"}) }()
	select {
	case err := <-handoff:
		require.NoError(t, err) // queued without waiting for delivery
	case <-time.After(defaultTimeout):
		t.Fatal("Prompt blocked behind the running turn instead of queueing")
	}
	assert.True(t, a.Running()) // turn one still owns the agent

	close(block)
	require.NoError(t, <-errCh)
	assert.Equal(t, []string{"one done", "two done"}, assistantTexts(a.state.Messages))
}

func TestPromptReentrantFromCallback(t *testing.T) {
	t.Parallel()

	// a nested Prompt from the running turn's own OnMessage callback must queue
	// behind the driver instead of waiting on it, which would deadlock the loop
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textOnly("one")},
		{Events: textOnly("two")}, // the nested prompt's own reply
	}}
	a := newTestAgent(nil, p, nil)

	var once sync.Once
	var nested error
	a.opts.OnMessage = append(a.opts.OnMessage, func(MessageInfo) {
		once.Do(func() { nested = a.Prompt(t.Context(), Input{Text: "nested"}) })
	})

	require.NoError(t, a.Prompt(t.Context(), Input{Text: "start"}))
	require.NoError(t, nested)
	assert.Equal(t, []string{"one", "two"}, assistantTexts(a.state.Messages))
}

func TestPromptConcurrentClaimsDrain(t *testing.T) {
	t.Parallel()

	// racing prompts must all land exactly once: one claims the drain under the
	// mutex, the rest seed behind it and its driver delivers everything queued
	const runs = 16
	turns := make([]llm.ScriptedTurn, 0, runs)
	for range runs { // spare turns: prompts that fold together need fewer calls
		turns = append(turns, llm.ScriptedTurn{Events: textOnly("ok")})
	}
	p := &llm.ScriptedProvider{Turns: turns}
	a := newTestAgent(nil, p, nil)

	errs := make([]error, runs)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = a.Prompt(t.Context(), Input{Text: fmt.Sprintf("p%02d", i)})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	got := userTexts(a.state.Messages)
	slices.Sort(got) // delivery order follows claim order, not submission index
	want := make([]string, 0, runs)
	for i := range runs {
		want = append(want, fmt.Sprintf("p%02d", i))
	}
	assert.Equal(t, want, got) // every prompt lands exactly once
}
