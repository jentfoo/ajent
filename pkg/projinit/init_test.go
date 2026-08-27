package projinit

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/agent"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSurvey(t *testing.T) {
	t.Parallel()

	t.Run("reads_then_starts_then_polls", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "README.md", "pkg/a/a.go", "cmd/b/b.go", "main.go")
		start, poll := startStub(), pollStub()
		r := New(Options{Cwd: dir, Registry: newRegistry(t, dir, start, poll)})

		in, err := r.Survey(t.Context())
		require.NoError(t, err)

		names := toolNames(agent.BeforeMessages(in.Before))
		require.NotEmpty(t, names)
		assert.Equal(t, "read", names[0])

		first := slices.Index(names, "agent_start")
		require.Positive(t, first)
		starts := strings.Count(strings.Join(names, " "), "agent_start")
		// every start precedes every poll, so the fan-out is parallel not serial
		assert.Equal(t, slices.Repeat([]string{"agent_start"}, starts), names[first:first+starts])
		assert.Equal(t, slices.Repeat([]string{"agent_poll"}, starts), names[first+starts:])
		assert.Equal(t, distillNew, in.Text)
		assert.True(t, in.Injected)
	})

	t.Run("build_task_is_first", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "pkg/a/a.go")
		start, poll := startStub(), pollStub()
		r := New(Options{Cwd: dir, Registry: newRegistry(t, dir, start, poll)})

		_, err := r.Survey(t.Context())
		require.NoError(t, err)

		calls := start.inputs()
		require.NotEmpty(t, calls)
		assert.Equal(t, buildTask, calls[0]["task"])
		for _, c := range calls[1:] {
			assert.Contains(t, c["task"], "Stay inside those paths")
		}
	})

	t.Run("summaries_reach_context", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "README.md", "pkg/a/a.go")
		r := New(Options{Cwd: dir, Registry: newRegistry(t, dir, startStub(), pollStub())})

		in, err := r.Survey(t.Context())
		require.NoError(t, err)
		assert.Contains(t, strings.Join(resultTexts(agent.BeforeMessages(in.Before)), "\n"), "summary of sub-1")
	})

	t.Run("existing_file_corrects", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "README.md", "AGENTS.md", "pkg/a/a.go")
		r := New(Options{Cwd: dir, Registry: newRegistry(t, dir, startStub(), pollStub())})

		in, err := r.Survey(t.Context())
		require.NoError(t, err)
		assert.Equal(t, distillUpdate, in.Text)
		assert.NotEqual(t, distillNew, in.Text)
		// both files are read, the existing one last so it sits nearest the instruction
		reads := resultTexts(agent.BeforeMessages(in.Before))
		require.GreaterOrEqual(t, len(reads), 2)
	})

	t.Run("retries_until_terminal", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "pkg/a/a.go")
		var mu sync.Mutex
		seen := map[string]int{}
		poll := &stubTool{name: "agent_poll", exec: func(call agent.ToolCall) agent.ToolResult {
			id := decodeID(call.Input)
			mu.Lock()
			seen[id]++
			n := seen[id]
			mu.Unlock()
			if n == 1 {
				return agent.ToolResult{
					Content: llm.BlockList{llm.TextBlock{Text: "still running after 1s"}},
					Details: map[string]string{"id": id, "status": "running"},
				}
			}
			return agent.ToolResult{
				Content: llm.BlockList{llm.TextBlock{Text: "summary of " + id}},
				Details: map[string]string{"id": id, "status": "done"},
			}
		}}
		r := New(Options{Cwd: dir, Registry: newRegistry(t, dir, startStub(), poll)})

		in, err := r.Survey(t.Context())
		require.NoError(t, err)
		texts := strings.Join(resultTexts(agent.BeforeMessages(in.Before)), "\n")
		assert.Contains(t, texts, "summary of sub-1")
		assert.NotContains(t, texts, "still running") // only the terminal pair is kept
	})

	t.Run("unknown_status_is_terminal", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "pkg/a/a.go")
		poll := &stubTool{name: "agent_poll", exec: func(agent.ToolCall) agent.ToolResult {
			return agent.ToolResult{Content: llm.BlockList{llm.TextBlock{Text: "who knows"}}}
		}}
		r := New(Options{Cwd: dir, Registry: newRegistry(t, dir, startStub(), poll)})

		in, err := r.Survey(t.Context()) // a missing status must not spin the poll loop
		require.NoError(t, err)
		assert.Contains(t, strings.Join(resultTexts(agent.BeforeMessages(in.Before)), "\n"), "who knows")
	})

	t.Run("refused_starts_are_named", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "pkg/a.go")
		start := &stubTool{name: "agent_start", exec: func(agent.ToolCall) agent.ToolResult {
			return agent.ToolResult{
				Content: llm.BlockList{llm.TextBlock{Text: "denied by user"}},
				IsError: true,
			}
		}}
		r := New(Options{Cwd: dir, Registry: newRegistry(t, dir, start, pollStub())})

		_, err := r.Survey(t.Context())
		// a refused spawn is not a missing tool; the notice must not say otherwise
		require.ErrorIs(t, err, ErrNoneStarted)
		require.NotErrorIs(t, err, ErrNoSubAgents)
		assert.Contains(t, err.Error(), "denied by user")
	})

	t.Run("call_ids_unique_across_runs", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "README.md", "pkg/a.go")
		r := New(Options{Cwd: dir, Registry: newRegistry(t, dir, startStub(), pollStub())})

		first, err := r.Survey(t.Context())
		require.NoError(t, err)
		second, err := r.Survey(t.Context())
		require.NoError(t, err)

		// Before stays in State; a repeated tool_use id 400s every later request
		seen := bulk.SliceToSet(callIDs(agent.BeforeMessages(first.Before)))
		ids := callIDs(agent.BeforeMessages(second.Before))
		require.NotEmpty(t, ids)
		for _, id := range ids {
			_, dup := seen[id]
			assert.False(t, dup, id)
		}
	})

	t.Run("reports_each_started_id", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "pkg/a.go")
		var got []string
		r := New(Options{
			Cwd: dir, Registry: newRegistry(t, dir, startStub(), pollStub()),
			Started: func(id string) { got = append(got, id) },
		})

		_, err := r.Survey(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{"sub-1", "sub-2"}, got) // the caller stops these by id
	})

	t.Run("without_read", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "README.md")
		// an empty registry has no read; drafting blind could overwrite a file
		// the model never saw
		_, err := New(Options{Cwd: dir, Registry: tools.New()}).Survey(t.Context())
		require.ErrorIs(t, err, ErrNoRead)
	})

	t.Run("without_subagents", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "README.md")
		r := New(Options{Cwd: dir, Registry: newRegistry(t, dir)})

		_, err := r.Survey(t.Context())
		require.ErrorIs(t, err, ErrNoSubAgents)

		_, err = New(Options{Cwd: dir}).Survey(t.Context())
		require.ErrorIs(t, err, ErrNoSubAgents)
	})
}

func TestTerminal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status string
		want   bool
	}{
		{"queued_waits", "queued", false},
		{"running_waits", "running", false},
		{"done_terminal", "done", true},
		{"error_terminal", "error", true},
		{"aborted_terminal", "aborted", true},
		{"missing_terminal", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := agent.ToolResult{Details: map[string]string{"status": tc.status}}
			assert.Equal(t, tc.want, terminal(res))
		})
	}
	assert.True(t, terminal(agent.ToolResult{})) // no Details at all
}
