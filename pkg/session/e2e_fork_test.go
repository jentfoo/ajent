package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestForkResumeAcrossBranches drives the full session-tree loop: a live agent
// grows a chain, we rewind via SetHead to an earlier point and grow a new tip,
// then kill and reopen — resume must come back on the persisted (new) branch
// with its exact context, while the old path stays reachable as another tip.
func TestForkResumeAcrossBranches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "2026-01-02T03-04-05Z-f00d.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	r := NewRecorder(w)

	set := &singleSet{tool: &noopTool{}}
	var mainLive []llm.Message
	a := agent.New(&agent.State{
		Model: llm.Model{ID: "test"},
		Tools: []string{"bash"},
	}, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("first reply")}}}, nil
		},
		Sinks: []agent.Sink{r.Sink(agent.NopSink{})},
		Tools: set,
		Env:   agent.Environment{Cwd: "/repo", OS: "linux/amd64", Date: "2024-01-02"},
		OnMessage: []func(agent.MessageInfo){
			r.Message,
			func(info agent.MessageInfo) { mainLive = append(mainLive, info.Message) },
		},
	})
	require.NoError(t, a.Prompt(t.Context(), agent.Input{Text: "first"}))
	tip1 := w.Head() // the assistant reply ends the first turn

	// rewind onto the *first user message* and grow an unrelated fork from there,
	// so the new branch still carries that prompt but not its old reply.
	branch1 := Branch(readEntries(t, p), tip1)
	w.SetHead(branch1[1].ID) // first user "first"
	forkA, ferr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "fork prompt")})
	require.NoError(t, ferr)
	require.NoError(t, w.Sync())
	tip2 := forkA.ID

	// crash without a graceful close; reopen from disk
	require.NoError(t, w.Close())

	entries, _, rerr := Read(p)
	require.NoError(t, rerr)

	resolve := func(key string) (llm.Model, error) { return llm.Model{ID: "test"}, nil }

	// resume reads the persisted HEAD: the new fork tip, not the file tail
	st, warns := State(Branch(entries, headFor(p, entries)), resolve)
	assert.Empty(t, warns)
	require.Len(t, st.Messages, 2) // "first" + "fork prompt", no old reply
	assert.Equal(t, llm.Text(llm.RoleUser, "first"), st.Messages[0])
	assert.Equal(t, llm.Text(llm.RoleUser, "fork prompt"), st.Messages[1])

	// both chains remain as tips; switching to tip1 restores the old context exactly
	tips := Tips(entries)
	ids := make([]string, len(tips))
	for i, tp := range tips {
		ids[i] = tp.ID
	}
	assert.ElementsMatch(t, []string{tip1, tip2}, ids)

	stOld, _ := State(Branch(entries, tip1), resolve)
	require.Len(t, stOld.Messages, len(mainLive))
	for i := range mainLive {
		// compare role and text (a JSON round trip turns nil Content into empty)
		assert.Equal(t, mainLive[i].Role, stOld.Messages[i].Role)
		assert.Equal(t, userText(mainLive[i]), userText(stOld.Messages[i]))
	}
}

// noopTool executes instantly so the loop never blocks on a real tool.
type noopTool struct{}

func (t *noopTool) Name() string                { return "bash" }
func (t *noopTool) Label(agent.ToolCall) string { return "bash: ..." }
func (t *noopTool) Description() string         { return "test tool" }
func (t *noopTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: "bash"} }
func (t *noopTool) Mode() agent.ExecutionMode {
	return agent.ModeSerial
}

// Execute returns an empty result immediately.
func (t *noopTool) Execute(_ context.Context, _ agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	return agent.ToolResult{Content: llm.BlockList{llm.TextBlock{Text: "ok"}}}, nil
}

// textTurn frames one assistant turn that emits a single text reply.
func textTurn(text string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventTextDelta, Text: text},
		{Type: llm.EventDone, StopReason: llm.StopEndTurn},
	}
}

// readEntries parses every entry of a transcript for tests.
func readEntries(t *testing.T, p string) []Entry {
	t.Helper()
	entries, warns, err := Read(p)
	require.NoError(t, err)
	assert.Empty(t, warns)
	return entries
}
