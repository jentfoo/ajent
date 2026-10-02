package command

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
)

// recordingSinkForShell captures tool start/output and notices for shell tests.
type recordingSinkForShell struct {
	mu      sync.Mutex
	starts  int
	full    bool
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
func (r *recordingSinkForShell) ToolStart(_ agent.ToolCall, _ string, full bool) func(agent.ToolResult) {
	r.mu.Lock()
	r.starts++
	r.full = full
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
	return NewStager(context.Background(), reg, sink), sink
}

func TestStagerRefusesDisabledBash(t *testing.T) {
	t.Parallel()

	reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "t"})
	require.NoError(t, err)
	reg.SetEnabled([]string{"read", "write", "edit"}) // bash off
	sink := &recordingSinkForShell{}
	s := NewStager(context.Background(), reg, sink)

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
	// (throttled login-shell startup) doesn't flake, a genuine hang still failing.
	require.Eventually(t, func() bool { return !s.Pending() }, 10*time.Second, time.Millisecond)
	msgs := s.Flush(t.Context())
	require.Len(t, msgs, 2)

	// each run lands as a single user text message in submission order
	assert.Equal(t, llm.RoleUser, msgs[0].Message.Role)
	assert.Contains(t, resultText(msgs[0].Message.Content), "User Ran: echo one")
	assert.Contains(t, resultText(msgs[0].Message.Content), "Output:")
	assert.Contains(t, resultText(msgs[0].Message.Content), "one")
	assert.Equal(t, llm.RoleUser, msgs[1].Message.Role)
	assert.Contains(t, resultText(msgs[1].Message.Content), "User Ran: echo two")
	_, ok := msgs[0].Message.Content[0].(llm.TextBlock)
	require.True(t, ok)
}

func TestStagerFlushWaitsForInFlight(t *testing.T) {
	t.Parallel()

	s, _ := newShellStager(t)
	s.Run("sleep 0.3; echo done", false)

	require.True(t, s.Pending())
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
	// budget clears the bash tool's 5s WaitDelay ceiling
	require.Eventually(t, func() bool { return !s.Pending() }, 10*time.Second, time.Millisecond)
	msgs := s.Flush(t.Context())
	require.Len(t, msgs, 1)
	// a non-zero exit is an ordinary staged result: the model sees the exit code
	// in the Output section, but it is not flagged as a turn failure
	assert.Contains(t, resultText(msgs[0].Message.Content), "exit status 3")
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
	assert.Contains(t, resultText(msgs[0].Message.Content), "interrupted by user")
}

func TestStagerExcludedRunFlushesNothingAndDoesNotWait(t *testing.T) {
	t.Parallel()

	s, _ := newShellStager(t)
	s.Run("sleep 1; echo late", true)
	require.True(t, s.Pending())

	// Flush returns immediately empty while the run keeps running: an excluded
	// result goes nowhere, so it must not hold the next prompt hostage.
	msgs := s.Flush(t.Context())
	assert.Empty(t, msgs)
	require.True(t, s.Pending())

	s.Cancel() // stop waiting on the sleep so the test ends promptly
	require.Eventually(t, func() bool { return !s.Pending() }, 3*time.Second, time.Millisecond)
}

func TestStagerFlushKeepsInFlightCancellable(t *testing.T) {
	t.Parallel()

	s, _ := newShellStager(t)
	s.Run("sleep 30; echo never", false)
	require.True(t, s.Pending())

	// Flush blocks on the run's done channel. Pending must stay true while it does,
	// otherwise Esc cannot interrupt a staged command once flushing begins.
	type flushResult struct{ msgs []agent.MessageInfo }
	resCh := make(chan flushResult, 1)
	go func() { resCh <- flushResult{s.Flush(t.Context())} }()

	require.Eventually(t, s.Pending, time.Second, time.Millisecond,
		"run stays pending while Flush waits on it")

	s.Cancel()

	var got flushResult
	require.Eventually(t, func() bool {
		select {
		case got = <-resCh:
			return true
		default:
		}
		return false
	}, 3*time.Second, time.Millisecond)

	require.Len(t, got.msgs, 1)
	assert.Contains(t, resultText(got.msgs[0].Message.Content), "interrupted by user")
}

func TestStagerRequestsFullToolStart(t *testing.T) {
	t.Parallel()

	s, sink := newShellStager(t)
	s.Run("echo hi", false)

	require.Eventually(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return !s.Pending() && sink.starts == 1 && sink.full
	}, time.Second, time.Millisecond)
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
			assert.Equal(t, llm.RoleUser, msg.Message.Role)
			assert.Contains(t, resultText(msg.Message.Content), c.want)
		})
	}
}

func TestStagerStagedEstimate(t *testing.T) {
	t.Parallel()

	t.Run("reports_after_run", func(t *testing.T) {
		s, _ := newShellStager(t)
		var mu sync.Mutex
		var got []int
		s.SetOnChange(func(est int) { mu.Lock(); got = append(got, est); mu.Unlock() })

		s.Run("echo "+strings.Repeat("payload ", 200), false)
		// the report fires after done closes, so a report arriving implies the run ended
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(got) > 0
		}, 3*time.Second, time.Millisecond)

		mu.Lock()
		last := got[len(got)-1]
		mu.Unlock()
		assert.Positive(t, last)
	})

	t.Run("excluded_run_not_counted", func(t *testing.T) {
		s, _ := newShellStager(t)
		var last int
		var mu sync.Mutex
		s.SetOnChange(func(est int) { mu.Lock(); last = est; mu.Unlock() })

		s.Run("echo "+strings.Repeat("secret ", 200), true) // `!!` never reaches context
		require.Eventually(t, func() bool { return !s.Pending() }, 3*time.Second, time.Millisecond)

		mu.Lock()
		defer mu.Unlock()
		assert.Zero(t, last)
	})

	t.Run("flush_clears", func(t *testing.T) {
		s, _ := newShellStager(t)
		var last int
		var mu sync.Mutex
		s.SetOnChange(func(est int) { mu.Lock(); last = est; mu.Unlock() })

		s.Run("echo hi", false)
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return last > 0 // the run's report must land before flush can clear it
		}, 3*time.Second, time.Millisecond)
		mu.Lock()
		staged := last
		mu.Unlock()
		require.Positive(t, staged)

		// the flushed results become the submission's to account for
		require.Len(t, s.Flush(t.Context()), 1)
		mu.Lock()
		defer mu.Unlock()
		assert.Zero(t, last)
	})
}

func TestStagerDiscard(t *testing.T) {
	t.Parallel()

	s, _ := newShellStager(t)
	var last int
	var mu sync.Mutex
	s.SetOnChange(func(est int) { mu.Lock(); last = est; mu.Unlock() })

	s.Run("echo hi", false)
	s.Run("sleep 30", false)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return last > 0
	}, 3*time.Second, time.Millisecond)

	s.Discard()
	require.Eventually(t, func() bool { return !s.Pending() }, 3*time.Second, time.Millisecond)
	assert.Empty(t, s.Flush(t.Context()))
	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, last)
}

// blockText builds a single-text-block list.
func blockText(text string) llm.BlockList {
	return llm.BlockList{llm.TextBlock{Text: text}}
}

// capturingBash records the input JSON each staged call hands the bash tool,
// optionally blocking so a run stays pending until released or cancelled.
type capturingBash struct {
	mu    sync.Mutex
	input []string
	user  bool // whether the latest Execute saw a user-initiated context
	block chan struct{}
}

func (b *capturingBash) Name() string                { return tools.ToolBash }
func (b *capturingBash) Label(agent.ToolCall) string { return "bash: ..." }
func (b *capturingBash) Description() string         { return "test tool" }
func (b *capturingBash) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: tools.ToolBash} }
func (b *capturingBash) Mode() agent.ExecutionMode   { return agent.ModeSerial }
func (b *capturingBash) Close()                      {}

func (b *capturingBash) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	b.mu.Lock()
	b.input = append(b.input, string(call.Input))
	b.user = tools.IsUserInitiated(ctx)
	b.mu.Unlock()
	if b.block != nil {
		select {
		case <-b.block:
		case <-ctx.Done():
		}
	}
	return agent.ToolResult{Content: llm.BlockList{llm.TextBlock{Text: "ok"}}}, nil
}

func (b *capturingBash) calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.Clone(b.input)
}

func (b *capturingBash) sawUserInitiated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.user
}

func TestStagerTimeoutReachesBashInput(t *testing.T) {
	t.Parallel()

	fake := &capturingBash{}
	reg := tools.New()
	reg.Register(fake, true)
	s := NewStager(context.Background(), reg, &recordingSinkForShell{})

	var p map[string]any
	decodeLast := func() {
		calls := fake.calls()
		require.NotEmpty(t, calls)
		require.NoError(t, json.Unmarshal([]byte(calls[len(calls)-1]), &p))
	}

	// the default zero ceiling runs uncapped: no timeout key at all, so the
	// model-facing two-minute default never applies to a user's own shell
	s.Run("echo one", false)
	require.Eventually(t, func() bool { return len(fake.calls()) == 1 }, time.Second, time.Millisecond)
	decodeLast()
	assert.Equal(t, "echo one", p["command"])
	assert.NotContains(t, p, "timeout")
	// the user-initiated mark is what keeps the tool from applying the
	// model-facing default on the empty timeout
	assert.True(t, fake.sawUserInitiated())

	// an explicit ceiling rounds up to whole seconds
	s.SetTimeout(1500 * time.Millisecond)
	s.Run("echo two", false)
	require.Eventually(t, func() bool { return len(fake.calls()) == 2 }, time.Second, time.Millisecond)
	decodeLast()
	assert.Equal(t, "echo two", p["command"])
	require.InDelta(t, 2, p["timeout"], 0)
}
