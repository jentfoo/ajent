package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// blockingTool blocks Execute until the context is cancelled, simulating a slow
// bash call that gets killed mid-turn.
type blockingTool struct {
	name    string
	entered chan struct{}
}

func (t *blockingTool) Name() string                { return t.name }
func (t *blockingTool) Label(agent.ToolCall) string { return t.name + ": ..." }
func (t *blockingTool) Description() string         { return "test tool" }
func (t *blockingTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: t.name} }
func (t *blockingTool) Mode() agent.ExecutionMode {
	return agent.ModeSerial
}

func (t *blockingTool) Close() {}

// Execute blocks until ctx is done, then returns an empty result.
func (t *blockingTool) Execute(ctx context.Context, _ agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	if t.entered != nil {
		close(t.entered)
		t.entered = nil
	}
	<-ctx.Done()
	return agent.ToolResult{}, nil
}

type singleSet struct{ tool agent.Tool }

func (s *singleSet) Get(name string) (agent.Tool, bool) { return s.tool, name == s.tool.Schema().Name }
func (s *singleSet) Schemas() []llm.ToolSchema          { return []llm.ToolSchema{s.tool.Schema()} }
func (s *singleSet) Names() []string                    { return []string{s.tool.Name()} }

// toolTurn frames one assistant turn ending in a single tool call.
func toolTurn(id, name string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventToolCallStart, Index: 0, ToolCallID: id, ToolName: name},
		{Type: llm.EventToolCallDelta, Index: 0, Text: `{}`},
		{Type: llm.EventToolCallEnd, Index: 0,
			Block: llm.ToolCallBlock{ID: id, Name: name, Input: json.RawMessage(`{}`)}},
		{Type: llm.EventDone, StopReason: llm.StopToolUse},
	}
}

// TestReusedCallIDResumeRebuilds runs a session whose provider reuses one
// tool-call id every step and verifies the rebuilt context equals what the loop
// saw: each exchange under its own id, nothing collapsed or dropped.
func TestReusedCallIDResumeRebuilds(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "session.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion, Model: "test"})
	require.NoError(t, err)
	r := NewRecorder(w)

	set := &singleSet{tool: &noopTool{}}
	// a fully framed text reply, so the final message carries a real block and
	// round-trips through the transcript without nil-vs-empty skew
	done := []llm.Event{
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: "done"},
		{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: "done"}},
		{Type: llm.EventDone, StopReason: llm.StopEndTurn},
	}
	scripted := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolTurn("c1", "bash")},
		{Events: toolTurn("c1", "bash")},
		{Events: done},
	}}
	var live []llm.Message
	a := agent.New(&agent.State{
		Model:     llm.Model{ID: "test"},
		Reasoning: llm.ReasoningConfig{},
		Tools:     []string{"bash"},
		Tokens:    tokens.New(llm.Model{ID: "test"}),
	}, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return scripted, nil },
		Sinks:    []agent.Sink{r.Sink(agent.NopSink{})},
		Tools:    set,
		Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64", Date: "2024-01-02"},
		OnMessage: []func(agent.MessageInfo){
			r.Message,
			func(info agent.MessageInfo) { live = append(live, info.Message) },
		},
	})

	require.NoError(t, a.Prompt(t.Context(), agent.Input{Text: "run it twice"}))
	require.NoError(t, w.Close())

	entries, warns, rerr := Read(p)
	require.NoError(t, rerr)
	assert.Empty(t, warns)
	branch := Branch(entries, Head(entries))
	st, swarns := State(branch, func(key string) (llm.Model, error) { return llm.Model{ID: "test"}, nil })
	assert.Empty(t, swarns)

	// rebuild equality: the transcript replays to exactly what the loop saw
	assert.Equal(t, live, st.Messages)
	assert.True(t, wellFormed(st.Messages))

	var callIDs []string
	for _, m := range st.Messages {
		for _, b := range m.Content {
			if tc, ok := b.(llm.ToolCallBlock); ok {
				callIDs = append(callIDs, tc.ID)
			}
		}
	}
	require.Len(t, callIDs, 2)
	assert.Equal(t, "c1", callIDs[0])   // the first exchange keeps the provider id
	assert.Equal(t, "c1_2", callIDs[1]) // the reuse was renamed before persist
}

// TestCrashResumeRebuildsState kills an agent mid-tool-dispatch and verifies the
// transcript rebuilds to exactly what it saw before the crash.
func TestCrashResumeRebuildsState(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "session.jsonl")
	// the starting model is recorded so a resume can stamp assistant provenance
	w, err := Create(p, SessionData{Version: sessionVersion, Model: "test"})
	require.NoError(t, err)
	r := NewRecorder(w)

	entered := make(chan struct{})
	set := &singleSet{tool: &blockingTool{name: "bash", entered: entered}}
	var live []llm.Message
	a := agent.New(&agent.State{
		Model:     llm.Model{ID: "test"},
		Reasoning: llm.ReasoningConfig{},
		Tools:     []string{"bash"},
		Tokens:    tokens.New(llm.Model{ID: "test"}),
	}, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: toolTurn("c1", "bash")}}}, nil
		},
		Sinks: []agent.Sink{r.Sink(agent.NopSink{})},
		Tools: set,
		Env:   agent.Environment{Cwd: "/repo", OS: "linux/amd64", Date: "2024-01-02"},
		OnMessage: []func(agent.MessageInfo){
			r.Message,
			func(info agent.MessageInfo) { live = append(live, info.Message) },
		},
	})

	done := make(chan error, 1)
	go func() { done <- a.Prompt(t.Context(), agent.Input{Text: "run it"}) }()
	require.Eventually(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond, "the tool must be mid-execution before the interrupt")
	a.Interrupt()
	require.NoError(t, <-done)

	// crash: close without a graceful final state, reopen from the file alone
	require.NoError(t, w.Close())

	entries, warns, rerr := Read(p)
	require.NoError(t, rerr)
	assert.Empty(t, warns) // every appended line was complete
	branch := Branch(entries, Head(entries))
	st, swarns := State(branch, func(key string) (llm.Model, error) { return llm.Model{ID: "test"}, nil })
	assert.Empty(t, swarns)

	// the rebuilt context is exactly what the loop saw, and every tool call has
	// a matching result so the next request stays well formed.
	assert.Equal(t, live, st.Messages)
	assert.True(t, wellFormed(st.Messages))
}
