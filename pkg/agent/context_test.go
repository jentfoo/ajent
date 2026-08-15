package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ctxCatcher records every ContextState the loop emits, for turn accounting tests.
type ctxCatcher struct {
	states []tokens.ContextState
}

func (c *ctxCatcher) TurnStart(TurnInfo) {}
func (c *ctxCatcher) Thinking(string)    {}
func (c *ctxCatcher) EndThinking()       {}
func (c *ctxCatcher) Text(string)        {}
func (c *ctxCatcher) EndText()           {}
func (c *ctxCatcher) ToolStart(ToolCall, string) func(ToolResult) {
	return func(ToolResult) {}
}
func (c *ctxCatcher) ToolOutput(string, string)   {}
func (c *ctxCatcher) Diff(string, string, string) {}
func (c *ctxCatcher) Usage(llm.Usage)             {}
func (c *ctxCatcher) Context(s tokens.ContextState) {
	c.states = append(c.states, s)
}
func (c *ctxCatcher) Notice(string, Level) {}
func (c *ctxCatcher) TurnEnd(TurnResult)   {}

// toolStep frames one assistant message that ends in a single tool call and
// reports its own input usage.
func toolStep(id string, input int) []llm.Event {
	return []llm.Event{
		{Type: llm.EventUsage, Usage: llm.Usage{Input: input}},
		{Type: llm.EventToolCallStart, Index: 0, ToolCallID: id, ToolName: "read"},
		{Type: llm.EventToolCallDelta, Index: 0, Text: `{"path":"a.go"}`},
		{Type: llm.EventToolCallEnd, Index: 0, Block: llm.ToolCallBlock{
			ID: id, Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)}},
	}
}

// TestMultiStepTurnDoesNotMultiplyContext is the regression for the old
// TurnResult.Usage over-report: a five-step turn must not report five times any
// single step's context, because each response snaps back to that provider's own
// reported input.
func TestMultiStepTurnDoesNotMultiplyContext(t *testing.T) {
	t.Parallel()

	const per = 1000 // every step reports the same real input
	var turns []llm.ScriptedTurn
	for i := 1; i <= 4; i++ {
		id := "c" + string(rune('0'+i))
		ev := toolStep(id, per)
		ev = append(ev, doneEvent())
		turns = append(turns, llm.ScriptedTurn{Events: ev})
	}
	final := textOnly("done")
	final = slices.Insert(final, 1, llm.Event{Type: llm.EventUsage, Usage: llm.Usage{Input: per}})
	turns = append(turns, llm.ScriptedTurn{Events: final})

	p := &llm.ScriptedProvider{Turns: turns}
	catch := &ctxCatcher{}
	st := &State{
		Model:     llm.Model{ID: "test", ContextWindow: 100000},
		Reasoning: llm.ReasoningConfig{},
		Tokens:    tokens.New(llm.Model{ID: "test", ContextWindow: 20000}),
	}
	a := newTestAgent(st, p, catch)
	a.opts.Tools = &mapSet{tools: map[string]Tool{"read": &stubTool{name: "read"}}}

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)

	// after the final response the bar is exact: promptExact + outputExact.
	finalState := catch.states[len(catch.states)-1]
	assert.False(t, finalState.Estimated)
	// each step reports Input=per; the final context must sit near per, never
	// five times it (the old bug summed every step's usage).
	assert.Less(t, finalState.Used, 3*per)
}

// TestFirstTurnContextIncludesSystemAndTools locks in the fix for the start-of-
// session undercount: before any exact provider report, the context bar must
// already carry the fixed system-prompt + tool-schema overhead that rides with
// every request — not just the appended message estimate.
func TestFirstTurnContextIncludesSystemAndTools(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}
	catch := &ctxCatcher{}
	st := &State{
		Model:     llm.Model{ID: "test", ContextWindow: 100000},
		Reasoning: llm.ReasoningConfig{},
		Tokens:    tokens.New(llm.Model{ID: "test", ContextWindow: 20000}),
	}
	a := newTestAgent(st, p, catch)
	set := &mapSet{tools: map[string]Tool{"read": &stubTool{name: "read"}}}
	a.opts.Tools = set
	a.opts.ProjectInstructions = []ProjectInstruction{{
		Path: "/repo/AGENTS.md", Body: "follow the repo rules carefully",
	}}

	require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))

	// what the request owes beyond its messages: system prompt + tool schemas.
	fixed := tokens.EstimateFixed(llm.Request{
		System: buildSystem(st, testEnv, a.opts.ProjectInstructions),
		Tools:  set.Schemas(),
	})
	require.Positive(t, fixed) // the fixtures must actually contribute overhead

	var sawBase bool
	for _, cs := range catch.states {
		if cs.Estimated && cs.Used >= fixed {
			sawBase = true
		}
	}
	assert.True(t, sawBase, "no emitted context bar carried the system+tools overhead")
}

// bigDelta is prose long enough that a single delta moves Used past the emit
// throttle, so each lands as its own progressive Context update.
func TestStreamingEmitsProgressiveContext(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("words ", 100) // ~600 bytes -> well over the token threshold
	var ev []llm.Event
	ev = append(ev, llm.Event{Type: llm.EventTextStart, Index: 0})
	for i := 0; i < 4; i++ {
		ev = append(ev, llm.Event{Type: llm.EventTextDelta, Index: 0, Text: big})
	}
	ev = append(ev,
		llm.Event{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: strings.Repeat(big, 4)}},
		doneEvent())

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: ev}}}
	catch := &ctxCatcher{}
	st := &State{
		Model:     llm.Model{ID: "test", ContextWindow: 100000},
		Reasoning: llm.ReasoningConfig{},
		Tokens:    tokens.New(llm.Model{ID: "test", ContextWindow: 20000}),
	}
	a := newTestAgent(st, p, catch)
	a.opts.Tools = &mapSet{tools: map[string]Tool{}}

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)

	// more than one Context emit means the bar advanced mid-stream, not just at end.
	assert.GreaterOrEqual(t, len(catch.states), 2)
}
