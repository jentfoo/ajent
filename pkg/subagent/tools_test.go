package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runTool executes a tool against the manager with canned args.
func exec(t *testing.T, tl agent.Tool, input any) (agent.ToolResult, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	return tl.Execute(t.Context(), agent.ToolCall{ID: "c1", Name: tl.Name(), Input: raw}, discardOutput{})
}

type discardOutput struct{}

func (discardOutput) Write(p []byte) (int, error)     { return len(p), nil }
func (discardOutput) ToolProgress(agent.ToolProgress) {}
func (discardOutput) Diff(string, string, string)     {}

// toolsManager wires a Manager over scripted turns for tool-level tests.
func toolsManager(t *testing.T, d *delayedProvider, timeout time.Duration) (*Manager, []agent.Tool) {
	t.Helper()
	opts := Options{
		PollTimeout: timeout,
	}
	if d != nil {
		opts.Provider = func(llm.Model) (llm.Provider, error) { return d, nil }
	} else {
		p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("summary text", llm.Usage{Input: 50, Output: 10})}})
		opts.Provider = p
	}
	m := New(opts)
	t.Cleanup(m.Close)
	return m, m.Tools()
}

func TestAgentStart(t *testing.T) {
	t.Parallel()

	t.Run("returns_id", func(t *testing.T) {
		_, tools := toolsManager(t, nil, time.Second)
		res, err := exec(t, tools[0], map[string]any{"task": "investigate x"})
		require.NoError(t, err)
		assert.False(t, res.IsError)
		var details struct {
			ID string `json:"id"`
		}
		require.NotNil(t, res.Details)
		b, err2 := json.Marshal(res.Details)
		require.NoError(t, err2)
		require.NoError(t, json.Unmarshal(b, &details))
		assert.Equal(t, "sub-1", details.ID)
	})

	t.Run("rejects_empty_task", func(t *testing.T) {
		m, _ := toolsManager(t, nil, time.Second)
		res, err := exec(t, m.Tools()[0], map[string]any{"task": "   "})
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})
}

func TestAgentPoll(t *testing.T) {
	t.Parallel()

	t.Run("returns_summary_on_completion", func(t *testing.T) {
		m, tools := toolsManager(t, nil, time.Second)
		id := m.Start("x", "")
		res, err := exec(t, tools[1], map[string]any{"id": id})
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Contains(t, textOf(res), "summary text")
		assert.Equal(t, map[string]string{"id": id, "status": "done"}, res.Details)
	})

	t.Run("accepts_bare_id", func(t *testing.T) {
		_, tools := toolsManager(t, nil, time.Second)
		_, serr := tools[0].Execute(t.Context(), agent.ToolCall{Name: "agent_start", Input: json.RawMessage(`{"task":"x"}`)}, discardOutput{})
		require.NoError(t, serr)
		res, err := exec(t, tools[1], map[string]any{"id": "1"})
		require.NoError(t, err)
		assert.False(t, res.IsError) // bare 1 resolves to sub-1
	})

	// a lone poll's payload lands directly under its own tool header, so naming it
	// again would be noise.
	t.Run("lone_poll_display_is_bare", func(t *testing.T) {
		m, tools := toolsManager(t, nil, time.Second)
		id := m.Start("x", "")
		res, err := exec(t, tools[1], map[string]any{"id": id})
		require.NoError(t, err)
		assert.NotContains(t, res.Display, "results:")
		assert.Equal(t, textOf(res), res.Display)
	})

	// agent_poll is ModeParallel: a batch commits every header at dispatch and each
	// payload only as its job finishes, so a batched result has to name its agent.
	t.Run("batched_poll_display_names_agent", func(t *testing.T) {
		g := &gatedProvider{} // held open so both polls overlap
		m := New(Options{
			Provider:    func(llm.Model) (llm.Provider, error) { return g, nil },
			PollTimeout: time.Second,
		})
		t.Cleanup(m.Close)
		tools := m.Tools()

		ids := []string{m.Start("a", ""), m.Start("b", "")}
		results := make([]agent.ToolResult, len(ids))
		errs := make([]error, len(ids)) // collected here; asserted on the test goroutine
		var wg sync.WaitGroup
		for i, id := range ids {
			call := agent.ToolCall{ID: "c" + strconv.Itoa(i), Name: "agent_poll",
				Input: json.RawMessage(`{"id":"` + id + `"}`)}
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = tools[1].Execute(t.Context(), call, discardOutput{})
			}()
		}
		require.Eventually(t, func() bool { // both polls registered before either result
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.polling == 2
		}, 2*time.Second, 5*time.Millisecond)
		g.releaseAll()
		wg.Wait()

		require.NoError(t, errors.Join(errs...))
		for i, res := range results {
			assert.Equal(t, ids[i]+" results:\n", res.Display[:len(ids[i])+len(" results:\n")])
			// only the human-facing copy is tagged; the model asked for this id
			assert.NotContains(t, textOf(res), "results:")
		}
	})

	t.Run("unknown_id_is_error", func(t *testing.T) {
		_, tools := toolsManager(t, nil, time.Second)
		res, err := exec(t, tools[1], map[string]any{"id": "sub-99"})
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})

	t.Run("timeout_reports_context_usage", func(t *testing.T) {
		d := &delayedProvider{turn: summaryTurn("slow", llm.Usage{}), release: make(chan struct{})}
		m, tools := toolsManager(t, d, 30*time.Millisecond)
		id := m.Start("x", "")
		res, err := exec(t, tools[1], map[string]any{"id": id})
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "still running after")
		// a host-driven poller reads the status rather than matching the prose
		assert.Equal(t, map[string]string{"id": id, "status": "running"}, res.Details)
	})

	t.Run("aborted", func(t *testing.T) {
		b := &blockingProvider{}
		m := New(Options{Provider: func(llm.Model) (llm.Provider, error) { return b, nil }})
		t.Cleanup(m.Close)
		id := m.Start("x", "")
		require.NoError(t, m.Stop(id))
		res, err := exec(t, m.Tools()[1], map[string]any{"id": id})
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Contains(t, textOf(res), "aborted")
		assert.Equal(t, map[string]string{"id": id, "status": "aborted"}, res.Details)
	})
}

func TestAgentListEmptyAndPopulated(t *testing.T) {
	t.Parallel()
	m, tools := toolsManager(t, nil, time.Second)
	res, err := exec(t, tools[2], map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, "(no sub-agents)", textOf(res))

	id := m.Start("x", "")
	res, err = exec(t, tools[2], map[string]any{})
	require.NoError(t, err)
	out := textOf(res)
	assert.Contains(t, out, id) // the job appears as a row
	assert.True(t, containsAny(out, "queued", "running", "done"))
}

func TestAgentToolsAreParallelAndLabeled(t *testing.T) {
	t.Parallel()
	_, tools := toolsManager(t, nil, time.Second)
	for _, tl := range tools {
		assert.Equal(t, agent.ModeParallel, tl.Mode())
	}
	assert.Contains(t, tools[0].Label(agent.ToolCall{}), "start")
}

// TestSubagentLabelsNameTheirTarget verifies the tool headers identify which agent
// was started or polled, not just the verb.
func TestSubagentLabelsNameTheirTarget(t *testing.T) {
	t.Parallel()
	_, tools := toolsManager(t, nil, time.Second)

	startLabel := func(task string) string {
		in, err := json.Marshal(map[string]string{"task": task})
		require.NoError(t, err)
		return tools[0].Label(agent.ToolCall{Input: in})
	}
	assert.Equal(t, "sub-agent: start inspect", startLabel("inspect\npkg"))
	long := strings.Repeat("x", 100)
	assert.Len(t, []rune(startLabel(long)), len([]rune("sub-agent: start "))+maxLabelLen)

	poll := func(id string) string {
		in, err := json.Marshal(map[string]string{"id": id})
		require.NoError(t, err)
		return tools[1].Label(agent.ToolCall{Input: in})
	}
	assert.Equal(t, "sub-agent: poll sub-2", poll("sub-2"))
	assert.Equal(t, "sub-agent: poll sub-3", poll("3")) // bare id normalizes

	// unparseable or empty args fall back to the generic label.
	assert.Contains(t, tools[0].Label(agent.ToolCall{}), "start")
	assert.Equal(t, "sub-agent: poll", tools[1].Label(agent.ToolCall{Input: json.RawMessage(`{"id":""}`)}))
}

func TestAgentStartDescriptionStatesContract(t *testing.T) {
	t.Parallel()
	_, tools := toolsManager(t, nil, time.Second)
	d := tools[0].Description()
	for _, want := range []string{"read", "grep", "find", "ls", "no session context"} {
		assert.Contains(t, d, want)
	}
}

// textOf joins a result's text blocks.
func textOf(r agent.ToolResult) string {
	var parts []string
	for _, b := range r.Content {
		if tb, ok := b.(llm.TextBlock); ok {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, "")
}

// containsAny reports whether s contains any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// startCall frames one agent_start tool-call block.
func startCall(id, task string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventToolCallStart, Index: 0, ToolCallID: id, ToolName: "agent_start"},
		{Type: llm.EventToolCallEnd, Index: 0, Block: llm.ToolCallBlock{
			ID: id, Name: "agent_start", Input: json.RawMessage(`{"task":"` + task + `"}`)}},
	}
}

// TestStartIDOrder asserts ids follow the order the model asked for the agents.
// agent_start is ModeParallel, so the dispatch goroutines race for the id counter:
// without the batch reservation the task submitted last routinely became sub-1.
func TestStartIDOrder(t *testing.T) {
	t.Parallel()
	g := &gatedProvider{}
	m := New(Options{Provider: func(llm.Model) (llm.Provider, error) { return g, nil }})
	t.Cleanup(m.Close)

	// one assistant message asking for three sub-agents, in order a, b, c
	var turn []llm.Event
	for i, task := range []string{"a", "b", "c"} {
		for _, ev := range startCall("call_"+task, task) {
			ev.Index = i
			if ev.Type == llm.EventToolCallEnd {
				ev.Block = llm.ToolCallBlock{ID: "call_" + task, Name: "agent_start",
					Input: json.RawMessage(`{"task":"` + task + `"}`)}
			}
			turn = append(turn, ev)
		}
	}
	turn = append(turn, llm.Event{Type: llm.EventDone, StopReason: llm.StopToolUse})

	parent := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: turn},
		{Events: summaryTurn("ok", llm.Usage{})},
	}}
	st := &agent.State{Model: llm.Model{ID: "parent", ContextWindow: 8000,
		Caps: llm.Capabilities{ParallelTools: true}}}
	a := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return parent, nil },
		Tools:    &toolSet{tools: m.Tools()},
		OnToolBatch: func(_ context.Context, calls []agent.ToolCall) {
			m.Reserve(calls)
		},
	})
	require.NoError(t, a.Prompt(t.Context(), agent.Input{Text: "fan out"}))

	jobs := m.List()
	require.Len(t, jobs, 3)
	got := make([]string, len(jobs))
	for i, j := range jobs {
		got[i] = j.ID + "=" + j.Task
	}
	assert.Equal(t, []string{"sub-1=a", "sub-2=b", "sub-3=c"}, got)
}
