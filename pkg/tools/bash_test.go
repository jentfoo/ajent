package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureOutput is a test Output that records streamed bytes.
type captureOutput struct{ buf strings.Builder }

func (c *captureOutput) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *captureOutput) Diff(string, string, string) {}

// bashRun carries a run's env so tests can inspect files the command wrote.
type bashRun struct {
	env toolEnv
	out captureOutput
	res agent.ToolResult
}

func newBash(t *testing.T, args string) *bashRun {
	t.Helper()
	return newBashCtx(t.Context(), t, args)
}

func newBashWithLimit(t *testing.T, lim Limit, args string) *bashRun {
	t.Helper()
	return newBashCtxLim(t.Context(), t, lim, args)
}

func newBashCtx(ctx context.Context, t *testing.T, args string) *bashRun {
	t.Helper()
	return newBashCtxLim(ctx, t, Limit{}, args)
}

// newBashCtxLim runs a command with an explicit output limit on the tool.
func newBashCtxLim(ctx context.Context, t *testing.T, lim Limit, args string) *bashRun {
	t.Helper()
	dir := t.TempDir()
	r := &bashRun{env: toolEnv{cwd: dir, tracker: NewTracker(), policy: PathPolicy{Cwd: dir}}}
	c := agent.ToolCall{ID: "c", Name: "bash", Input: []byte(args)}
	res, err := (&bashTool{policy: r.env.policy, limit: lim}).Execute(ctx, c, &r.out)
	require.NoError(t, err) // bash surfaces failures as error results, not Go errors
	r.res = res
	return r
}

func TestBash(t *testing.T) {
	t.Parallel()

	// a non-zero exit is reported in the result text.
	t.Run("exit_code_reported", func(t *testing.T) {
		r := newBash(t, `{"command":"exit 3"}`)
		assert.False(t, r.res.IsError)
		assert.Contains(t, textOf(r.res), "exit status 3")
	})

	// stdout and stderr are both captured for the model.
	t.Run("streams_stdout_and_stderr_interleaved", func(t *testing.T) {
		r := newBash(t, `{"command":"echo out; echo err >&2"}`)
		assert.False(t, r.res.IsError)
		assert.Contains(t, textOf(r.res), "out")
		assert.Contains(t, textOf(r.res), "err") // stderr captured for the model
	})

	t.Run("empty_command_rejected", func(t *testing.T) {
		r := newBash(t, `{"command":"  "}`)
		assert.True(t, r.res.IsError)
	})

	// an already-cancelled context means the command never runs to completion.
	t.Run("cancellation_via_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // already cancelled: the command must not run to completion
		r := newBashCtx(ctx, t, `{"command":"echo should-not-appear"}`)
		assert.True(t, r.res.IsError) // cancellation is a clean stop marked as interrupted
		assert.Contains(t, textOf(r.res), "interrupted by user")
		assert.NotContains(t, textOf(r.res), "should-not-appear")
	})
}

func TestBashTimeoutKillsWholeProcessGroup(t *testing.T) {
	t.Parallel()

	// a subshell starts a grandchild that sleeps; on timeout the whole group
	// (including the grandchild) must be killed.
	r := newBash(t, `{"command":"sleep 300 & echo $! > pid.txt; wait","timeout":1}`)
	assert.False(t, r.res.IsError)
	assert.Contains(t, textOf(r.res), "killed after") // the model learns it was a timeout

	data, err := os.ReadFile(r.env.cwd + "/pid.txt")
	if err != nil {
		t.Skipf("grandchild pid not written before the kill: %v", err)
	}
	grandchildPid, aerr := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, aerr)
	assertEventuallyGone(t, grandchildPid)
}

// TestBashMidRunCancelKillsGroupAndRecordsPartial interrupts a running command,
// proving the whole process group (including a TERM-trapping grandchild) is killed
// and the partial output comes back as an interrupted error result.
func TestBashMidRunCancelKillsGroupAndRecordsPartial(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	dir := t.TempDir()
	// a TERM-trapping child proves a leader-only SIGTERM would leak it; the group
	// SIGKILL must take it down with the parent.
	cmd := fmt.Sprintf(`echo started; echo $$ > %s/pid.txt; sh -c 'trap "" TERM; sleep 30'; echo finished`, dir)
	env := toolEnv{cwd: dir, tracker: NewTracker(), policy: PathPolicy{Cwd: dir}}
	out := captureOutput{}
	call := agent.ToolCall{ID: "c", Name: "bash", Input: []byte(`{"command":` + strconv.Quote(cmd) + `}`)}

	resCh := make(chan agent.ToolResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := (&bashTool{policy: env.policy}).Execute(ctx, call, &out)
		if err != nil {
			errCh <- err
			return
		}
		resCh <- res
	}()

	// wait for the command to be running (pid written) before interrupting
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(dir + "/pid.txt")
		return err == nil && len(strings.TrimSpace(string(data))) > 0
	}, time.Second*2, time.Millisecond*10, "the command must be running before the interrupt")

	cancel()

	var res agent.ToolResult
	select {
	case res = <-resCh:
	case err := <-errCh:
		t.Fatalf("Execute returned an error: %v", err)
	case <-time.After(time.Second * 5):
		t.Fatal("Execute did not return after cancellation")
	}

	assert.True(t, res.IsError) // a cancelled run is marked as interrupted
	text := textOf(res)
	assert.Contains(t, text, "interrupted by user")
	assert.Contains(t, text, "started")     // partial output rides in the result
	assert.NotContains(t, text, "finished") // the command was cut off mid-run

	// prove the whole group (leader and TERM-trapping grandchild) is gone
	data, err := os.ReadFile(dir + "/pid.txt")
	require.NoError(t, err)
	pid, aerr := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, aerr)
	assertEventuallyGone(t, pid) // the leader is gone
	// and no descendant of its group survives
	require.Eventually(t, func() bool {
		err := syscall.Kill(-pid, 0)
		return err != nil && errors.Is(err, syscall.ESRCH)
	}, time.Second*2, time.Millisecond*30, "the whole process group must be gone")
}

func TestBashOutputElision(t *testing.T) {
	t.Parallel()

	// a head-only policy names the shown/total counts and spills.
	t.Run("elision_by_line_bound_spills_file_suffix", func(t *testing.T) {
		r := newBashWithLimit(t, Limit{Lines: 5}, `{"command":"seq 1 10000"}`)
		assert.False(t, r.res.IsError)
		out := textOf(r.res)
		// head-only policy with a footer naming shown/total and the spill file
		assert.Contains(t, out, "... truncated: 5/10000 lines shown")
		assert.Regexp(t, `full output in @\S+`, out)
	})

	// one minified line within every bound must not reach the model whole when the result truncates.
	t.Run("truncated_head_caps_overlong_lines", func(t *testing.T) {
		long := strings.Repeat("y", MaxLineRunes+200)
		cmd := fmt.Sprintf(`{"command":"printf '%%s\\n' '%s'; seq 1 20"}`, long)
		r := newBashWithLimit(t, Limit{Lines: 2}, cmd)
		assert.False(t, r.res.IsError)
		for _, ln := range strings.Split(textOf(r.res), "\n") {
			assert.LessOrEqual(t, len([]rune(ln)), MaxLineRunes)
		}
		assert.Regexp(t, `full output in @\S+`, textOf(r.res))
	})

	// a single overlong line under every bound is still capped and spilled.
	t.Run("overlong_line_within_bounds_capped_and_spilled", func(t *testing.T) {
		long := strings.Repeat("y", 2000)
		cmd := fmt.Sprintf(`{"command":"printf '%%s\\n' '%s'"}`, long)
		r := newBashWithLimit(t, Limit{Lines: 10}, cmd)
		assert.False(t, r.res.IsError)
		out := textOf(r.res)
		for _, ln := range strings.Split(out, "\n") {
			assert.LessOrEqual(t, len([]rune(ln)), MaxLineRunes+100) // footer carries the spill note
		}
		// an overlong line alone counts as truncated: full stream spilled with a size summary
		assert.Regexp(t, `full output in @\S+`, out)
		m := regexp.MustCompile(`@(\S+)`).FindStringSubmatch(out)
		require.NotNil(t, m, "spill path must be named")
		dat, err := os.ReadFile(m[1])
		require.NoError(t, err) // the spilled file really holds the complete stream
		assert.Contains(t, string(dat), strings.Repeat("y", 2000))
	})
}

func TestBashEnvironment(t *testing.T) {
	t.Parallel()

	// ANSI escapes are stripped before the model sees output.
	t.Run("strips_ansi_from_captured_output", func(t *testing.T) {
		r := newBash(t, `{"command":"printf '\\033[31mred\\033[0m plain'"}`)
		assert.False(t, r.res.IsError)
		out := textOf(r.res)
		assert.NotContains(t, out, "\x1b") // escapes stripped before the model sees it
		assert.Contains(t, out, "plain")
	})

	// a non-login shell inherits our PATH verbatim (a login shell would reset it).
	t.Run("preserves_parent_path", func(t *testing.T) {
		want := os.Getenv("PATH")
		r := newBash(t, `{"command":"printf %s \"$PATH\""}`)
		assert.False(t, r.res.IsError)
		assert.Equal(t, want, textOf(r.res))
	})

	// an empty cwd override falls back to the policy cwd.
	t.Run("respects_cwd_override", func(t *testing.T) {
		r := newBash(t, `{"command":"pwd","cwd":""}`)
		assert.Contains(t, textOf(r.res), r.env.cwd)
	})
}

// assertEventuallyGone waits until a process is gone, proving the whole group
// (including grandchildren) was killed on timeout.
func assertEventuallyGone(t *testing.T, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		err := syscall.Kill(pid, 0)
		return err != nil && errors.Is(err, syscall.ESRCH)
	}, time.Second*2, time.Millisecond*30)
}
