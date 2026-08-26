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

	s.Run("echo hi", false)
	sink.mu.Lock()
	first := sink.notices[0]
	sink.mu.Unlock()
	assert.Contains(t, first, "bash")
}

func TestStagerRunsAndFlushesInOrder(t *testing.T) {
	t.Parallel()

	s, sink := newShellStager(t)
	s.Run("echo one", false)
	s.Run("echo two", false)

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
	require.Len(t, msgs, 2, "one user message per run")

	// each run lands as a single user text message in submission order
	assert.Equal(t, llm.RoleUser, msgs[0].Role)
	assert.Contains(t, resultText(msgs[0].Content), "User Ran: echo one")
	assert.Contains(t, resultText(msgs[0].Content), "Output:")
	assert.Contains(t, resultText(msgs[0].Content), "one")
	assert.Equal(t, llm.RoleUser, msgs[1].Role)
	assert.Contains(t, resultText(msgs[1].Content), "User Ran: echo two")
	_, ok := msgs[0].Content[0].(llm.TextBlock)
	require.True(t, ok, "the staged result is a plain text block, not a tool result")
}

func TestStagerFlushWaitsForInFlight(t *testing.T) {
	t.Parallel()

	s, _ := newShellStager(t)
	s.Run("sleep 0.3; echo done", false)

	require.True(t, s.Pending(), "the sleep is still running")
	start := time.Now()
	msgs := s.Flush(t.Context())
	elapsed := time.Since(start)
	require.Len(t, msgs, 1)
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
			s.Run(cmd, false)
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
	s.Run("exit 3", false)
	// budget clears the bash tool's 5s WaitDelay ceiling (see TestStagerRunsAndFlushesInOrder).
	require.Eventually(t, func() bool { return !s.Pending() }, 10*time.Second, time.Millisecond)
	msgs := s.Flush(t.Context())
	require.Len(t, msgs, 1)
	// a non-zero exit is an ordinary staged result: the model sees the exit code
	// in the Output section, but it is not flagged as a turn failure
	assert.Contains(t, resultText(msgs[0].Content), "exit status 3")
}

func TestStagerCancelStagesPartial(t *testing.T) {
	t.Parallel()

	s, _ := newShellStager(t)
	s.Run("sleep 30; echo never", false)
	require.True(t, s.Pending())

	s.Cancel()
	require.Eventually(t, func() bool { return !s.Pending() }, 3*time.Second, time.Millisecond)
	msgs := s.Flush(t.Context())
	require.Len(t, msgs, 1)

	// a cancelled command stages an interrupted marker so the model sees it was cut off
	assert.Contains(t, resultText(msgs[0].Content), "interrupted by user")
}

func TestStagerExcludedRunFlushesNothingAndDoesNotWait(t *testing.T) {
	t.Parallel()

	s, _ := newShellStager(t)
	s.Run("sleep 1; echo late", true)
	require.True(t, s.Pending(), "the excluded run is still in flight")

	// Flush returns immediately empty while the run keeps running: an excluded
	// result goes nowhere, so it must not hold the next prompt hostage.
	msgs := s.Flush(t.Context())
	assert.Empty(t, msgs)
	require.True(t, s.Pending(), "the excluded run is still tracked after Flush")

	s.Cancel() // stop waiting on the sleep so the test ends promptly
	require.Eventually(t, func() bool { return !s.Pending() }, 3*time.Second, time.Millisecond)
}

// fullSinkForShell records whether ToolStartFull was chosen for a staged run.
type fullSinkForShell struct {
	*recordingSinkForShell
	fullStarts int
}

func (f *fullSinkForShell) ToolStartFull(_ agent.ToolCall, _ string) func(agent.ToolResult) {
	f.mu.Lock()
	f.fullStarts++
	f.starts++
	f.mu.Unlock()
	return func(res agent.ToolResult) { f.mu.Lock(); f.done = append(f.done, res); f.mu.Unlock() }
}

func TestStagerPrefersFullToolStart(t *testing.T) {
	t.Parallel()

	s, sink := newShellStager(t)
	full := &fullSinkForShell{recordingSinkForShell: sink}
	s.sink = full // the stager's sink is fixed at construction; swap to a fuller one
	s.Run("echo hi", false)

	require.Eventually(t, func() bool {
		full.mu.Lock()
		defer full.mu.Unlock()
		return !s.Pending() && full.fullStarts == 1
	}, time.Second, time.Millisecond,
		"staged shell must route through ToolStartFull when the sink offers it")
}

func TestShellUserMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cmd     string
		content llm.BlockList
		want    string // substring expected in the rendered text
	}{
		{"normal_output", "echo hi", blockText("hello\n"), "User Ran: echo hi\n\nOutput:\nhello"},
		{"empty_output", "true", llm.BlockList{}, "(no output)"},
		{"multiline_command", "grep x f; wc -l", blockText("a\nb\nc"), "User Ran: grep x f; wc -l"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := shellUserMessage(c.cmd, agent.ToolResult{Content: c.content})
			assert.Equal(t, llm.RoleUser, msg.Role)
			assert.Contains(t, resultText(msg.Content), c.want)
		})
	}
}

// blockText builds a single-text-block list.
func blockText(text string) llm.BlockList {
	return llm.BlockList{llm.TextBlock{Text: text}}
}
