package command

import (
	"sync"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingSinkForShell captures tool start/output and notices for shell tests.
type recordingSinkForShell struct {
	mu      sync.Mutex
	starts  int
	outputs []string
	notices []string
	done    []agent.ToolResult
}

func (r *recordingSinkForShell) TurnStart(agent.TurnInfo) {}
func (r *recordingSinkForShell) UserPrompt(string)        {}
func (r *recordingSinkForShell) Thinking(string)          {}
func (r *recordingSinkForShell) EndThinking()             {}
func (r *recordingSinkForShell) Text(string)              {}
func (r *recordingSinkForShell) EndText()                 {}
func (r *recordingSinkForShell) ToolStart(_ agent.ToolCall, _ string) func(agent.ToolResult) {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	return func(res agent.ToolResult) { r.mu.Lock(); r.done = append(r.done, res); r.mu.Unlock() }
}
func (r *recordingSinkForShell) ToolOutput(_, d string) {
	r.mu.Lock()
	r.outputs = append(r.outputs, d)
	r.mu.Unlock()
}
func (r *recordingSinkForShell) ToolProgress(agent.ToolProgress) {}
func (r *recordingSinkForShell) Diff(string, string, string)     {}
func (r *recordingSinkForShell) Usage(llm.Usage)                 {}
func (r *recordingSinkForShell) Context(tokens.ContextState)     {}
func (r *recordingSinkForShell) Notice(msg string, _ agent.Level) {
	r.mu.Lock()
	r.notices = append(r.notices, msg)
	r.mu.Unlock()
}
func (r *recordingSinkForShell) TurnEnd(agent.TurnResult) {}

// newShellStager builds a real bash-backed registry and stager for tests.
func newShellStager(t *testing.T) (*Stager, *recordingSinkForShell) {
	t.Helper()
	reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "shelltest"})
	require.NoError(t, err)
	sink := &recordingSinkForShell{}
	return NewStager(reg, sink), sink
}

func TestStagerRefusesDisabledBash(t *testing.T) {
	t.Parallel()

	reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "t"})
	require.NoError(t, err)
	reg.SetEnabled([]string{"read", "write", "edit"}) // bash off
	sink := &recordingSinkForShell{}
	s := NewStager(reg, sink)

	s.Run("echo hi")
	require.Eventually(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.notices) > 0
	}, time.Second, time.Millisecond,
		"disabled bash must refuse with a notice")
	sink.mu.Lock()
	first := sink.notices[0]
	sink.mu.Unlock()
	assert.Contains(t, first, "bash")
}

func TestStagerRunsAndFlushesInOrder(t *testing.T) {
	t.Parallel()

	s, sink := newShellStager(t)
	s.Run("echo one")
	s.Run("echo two")

	// both start streaming immediately
	require.Eventually(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.starts == 2
	}, time.Second, time.Millisecond,
		"both staged commands must start immediately")

	// budget clears the bash tool's 5s WaitDelay ceiling so a slow CI runner
	// (throttled login-shell startup) doesn't flake; a genuine hang still fails.
	require.Eventually(t, func() bool { return !s.Pending() }, 10*time.Second, time.Millisecond,
		"quick commands finish")
	msgs := s.Flush(t.Context())
	require.Len(t, msgs, 4, "one assistant+user pair per run")

	// pair 0: assistant tool call, pair 1: user tool result; then run 2
	assert.Equal(t, llm.RoleAssistant, msgs[0].Role)
	assert.Equal(t, llm.RoleUser, msgs[1].Role)
	assert.Equal(t, llm.RoleAssistant, msgs[2].Role)
	assert.Equal(t, llm.RoleUser, msgs[3].Role)

	// provenance marks every result as a shell source
	for _, m := range []llm.Message{msgs[1], msgs[3]} {
		tr := m.Content[0].(llm.ToolResultBlock)
		assert.Equal(t, "shell", tr.Details.(shellProvenance).Source)
	}
}

func TestStagerFlushWaitsForInFlight(t *testing.T) {
	t.Parallel()

	s, _ := newShellStager(t)
	s.Run("sleep 0.3; echo done")

	require.True(t, s.Pending(), "the sleep is still running")
	start := time.Now()
	msgs := s.Flush(t.Context())
	elapsed := time.Since(start)
	require.Len(t, msgs, 2)
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond)
	require.False(t, s.Pending())
}

func TestStagerEmptyCommandNoticesAndRunsNothing(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, cmd string }{
		{"bare_empty", ""},
		{"only_spaces", "   "},
		{"only_tab", "\t"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := c.cmd
			s, sink := newShellStager(t)
			s.Run(cmd)
			require.Eventually(t, func() bool {
				sink.mu.Lock()
				defer sink.mu.Unlock()
				return len(sink.notices) > 0
			}, time.Second, time.Millisecond)
			sink.mu.Lock()
			first := sink.notices[0]
			sink.mu.Unlock()
			assert.Contains(t, first, "empty")
			require.False(t, s.Pending())
			msgs := s.Flush(t.Context())
			assert.Empty(t, msgs)
		})
	}
}

func TestStagerNonZeroExitStagesAsError(t *testing.T) {
	t.Parallel()

	s, _ := newShellStager(t)
	s.Run("exit 3")
	// budget clears the bash tool's 5s WaitDelay ceiling (see TestStagerRunsAndFlushesInOrder).
	require.Eventually(t, func() bool { return !s.Pending() }, 10*time.Second, time.Millisecond)
	msgs := s.Flush(t.Context())
	require.Len(t, msgs, 2)
	tr := msgs[1].Content[0].(llm.ToolResultBlock)
	// a non-zero exit is an ordinary staged result: the model sees the exit code
	// in the content, but it is not flagged as a turn failure
	assert.Contains(t, firstResultText(tr), "exit status 3")
}

func TestStagerCancelStagesPartial(t *testing.T) {
	t.Parallel()

	s, _ := newShellStager(t)
	s.Run("sleep 30; echo never")
	require.True(t, s.Pending())

	s.Cancel()
	require.Eventually(t, func() bool { return !s.Pending() }, 3*time.Second, time.Millisecond)
	msgs := s.Flush(t.Context())
	require.Len(t, msgs, 2)
}

// firstResultText extracts the text of a tool result block.
func firstResultText(tr llm.ToolResultBlock) string {
	for _, b := range tr.Content {
		if tb, ok := b.(llm.TextBlock); ok && tb.Text != "" {
			return tb.Text
		}
	}
	return ""
}
