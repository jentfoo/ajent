package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

	// a non-zero exit is reported in the result text
	t.Run("exit_code_reported", func(t *testing.T) {
		r := newBash(t, `{"command":"exit 3"}`)
		assert.False(t, r.res.IsError)
		assert.Contains(t, textOf(r.res), "exit status 3")
	})

	// a signal death names the signal instead of a bogus exit code
	t.Run("signal_death_names_signal", func(t *testing.T) {
		r := newBash(t, `{"command":"kill -SEGV $$"}`)
		assert.False(t, r.res.IsError)
		out := textOf(r.res)
		assert.Contains(t, out, "signal: segmentation fault")
		assert.NotContains(t, out, "exit status -1")
	})

	// stdout and stderr are both captured for the model
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

	// an already-cancelled context means the command never runs to completion
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
	if testing.Short() {
		t.Skip("-short mode")
	}
	t.Parallel()

	// a subshell starts a grandchild that sleeps. On timeout the whole group
	// (including the grandchild) must be killed.
	r := newBash(t, `{"command":"sleep 300 & echo $! > pid.txt; wait","timeout":1}`)
	assert.False(t, r.res.IsError)
	out := textOf(r.res)
	assert.Contains(t, out, "killed after") // the model learns it was a timeout
	// our own SIGKILL is not an exit status: the timeout note already says why
	assert.NotContains(t, out, "exit status")

	data, err := os.ReadFile(r.env.cwd + "/pid.txt")
	if err != nil {
		t.Skipf("grandchild pid not written before the kill: %v", err)
	}
	grandchildPid, aerr := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, aerr)
	assertEventuallyGone(t, grandchildPid)
}

func TestBashMidRunCancelKillsGroupAndRecordsPartial(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	dir := t.TempDir()
	// a TERM-trapping child proves a leader-only SIGTERM would leak it. The group
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

	// group-kill + reap can take variable real time, so poll rather than fix a short cap.
	var res agent.ToolResult
	require.Eventually(t, func() bool {
		select {
		case r := <-resCh:
			res = r
			return true
		default:
		}
		return false
	}, 10*time.Second, time.Millisecond*10)
	// Execute resolves through res or err; both are buffered(1) so a non-blocking probe is safe.
	select {
	case err := <-errCh:
		t.Fatalf("Execute returned an error: %v", err)
	default:
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

	// a head-only policy names the shown/total counts and spills
	t.Run("elision_by_line_bound_spills_file_suffix", func(t *testing.T) {
		r := newBashWithLimit(t, Limit{Lines: 5}, `{"command":"seq 1 10000"}`)
		assert.False(t, r.res.IsError)
		out := textOf(r.res)
		// head-only policy with a footer naming shown/total and the spill file
		assert.Contains(t, out, "... truncated: 5/10000 lines shown")
		assert.Regexp(t, `full output in @\S+`, out)
	})

	// one minified line within every bound must not reach the model whole when the result truncates
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

	// a single overlong line under every bound is still capped and spilled
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

	// ANSI escapes are stripped before the model sees output
	t.Run("strips_ansi_from_captured_output", func(t *testing.T) {
		r := newBash(t, `{"command":"printf '\\033[31mred\\033[0m plain'"}`)
		assert.False(t, r.res.IsError)
		out := textOf(r.res)
		assert.NotContains(t, out, "\x1b") // escapes stripped before the model sees it
		assert.Contains(t, out, "plain")
	})

	// a sequence split over two writes arrives as two chunks: still no leak. The
	// sleep keeps them separate reads, but coalesced they would strip just the same.
	t.Run("strips_ansi_split_across_chunks", func(t *testing.T) {
		r := newBash(t, `{"command":"printf '\\033[3'; sleep 0.2; printf '1mred\\033[m done'"}`)
		assert.False(t, r.res.IsError)
		out := textOf(r.res)
		assert.NotContains(t, out, "\x1b")
		assert.NotContains(t, out, "1m") // the tail of a half-received sequence
		assert.Contains(t, out, "red done")
	})

	// a non-login shell inherits our PATH verbatim (a login shell would reset it)
	t.Run("preserves_parent_path", func(t *testing.T) {
		want := os.Getenv("PATH")
		r := newBash(t, `{"command":"printf %s \"$PATH\""}`)
		assert.False(t, r.res.IsError)
		assert.Equal(t, want, textOf(r.res))
	})

	// an empty cwd override falls back to the policy cwd
	t.Run("respects_cwd_override", func(t *testing.T) {
		r := newBash(t, `{"command":"pwd","cwd":""}`)
		assert.Contains(t, textOf(r.res), r.env.cwd)
	})
}

func TestSyncSink(t *testing.T) {
	t.Parallel()

	const stream = "a\x1b[31mred\x1b[0m\nb\x1b]0;title\x07c\n"
	const want = "ared\nbc\n"

	run := func(chunks ...string) (live, captured string) {
		var mu sync.Mutex
		var out captureOutput
		var head strings.Builder
		sink := &syncSink{mu: &mu, out: &out, w: &head}
		for _, c := range chunks {
			_, err := sink.Write([]byte(c))
			require.NoError(t, err)
		}
		return out.buf.String(), head.String()
	}

	t.Run("stdout_esc_stderr_rest", func(t *testing.T) {
		live, captured := run("a\x1b", "[31mred")
		assert.Equal(t, "ared", live)
		assert.Equal(t, "ared", captured)
	})

	t.Run("split_at_every_boundary", func(t *testing.T) {
		for i := 0; i <= len(stream); i++ {
			live, captured := run(stream[:i], stream[i:])
			assert.Equal(t, want, live)
			assert.Equal(t, want, captured)
		}
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

func TestBashDescription(t *testing.T) {
	t.Parallel()

	t.Run("omits_commands_when_unset", func(t *testing.T) {
		got := (&bashTool{}).Description()
		assert.NotContains(t, got, "Example available commands")
	})

	t.Run("lists_example_commands", func(t *testing.T) {
		got := (&bashTool{shellExamples: []string{"ls", "diff", "wc"}}).Description()
		assert.Contains(t, got, "Example available commands: ls, diff, wc")
	})
}

func TestBuiltinsShellCommands(t *testing.T) {
	t.Parallel()

	reg, err := Builtins(Options{Cwd: t.TempDir(), ShellCommands: []string{"diff", "wc"}})
	require.NoError(t, err)

	var desc string
	for _, s := range reg.Schemas() {
		if s.Name == ToolBash {
			desc = s.Description
		}
	}
	assert.Contains(t, desc, "Example available commands: diff, wc")
}

func TestBashTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested time.Duration
		userRun   bool
		want      time.Duration
	}{
		{"model_default", 0, false, defaultBashTimeout},
		{"user_uncapped", 0, true, 0},
		{"model_within_max", 30 * time.Second, false, 30 * time.Second},
		{"model_clamped", time.Hour, false, maxBashTimeout},
		{"user_explicit_any_ceiling", time.Hour, true, time.Hour},
		{"negative_model_default", -5 * time.Second, false, defaultBashTimeout},
		{"negative_user_uncapped", -5 * time.Second, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, bashTimeout(tc.requested, tc.userRun))
		})
	}
}

func TestBashBackground(t *testing.T) {
	t.Parallel()

	// a background call returns at once with the pid and both log paths
	t.Run("returns_pid_and_log_paths", func(t *testing.T) {
		dir := t.TempDir()
		r := newBash(t, fmt.Sprintf(`{"command":"cd %s && echo hi; echo boom >&2","background":true}`, dir))
		assert.False(t, r.res.IsError)
		out := textOf(r.res)
		assert.Regexp(t, `started in background, pid \d+`, out)
		assert.Regexp(t, `stdout log: \S+bash-stdout`, out)
		assert.Regexp(t, `stderr log: \S+bash-stderr`, out)
		// the tool came back without waiting, so the result must not hold the
		// command's own output
		assert.NotContains(t, out, "hi")
	})

	// output lands in the named files and the stop note follows a natural exit
	t.Run("logs_capture_output_and_stop_note", func(t *testing.T) {
		r := newBash(t, `{"command":"echo hi; echo boom >&2; exit 7","background":true}`)
		assert.False(t, r.res.IsError)
		out := textOf(r.res)
		outPath := regexp.MustCompile(`stdout log: (\S+)`).FindStringSubmatch(out)
		errPath := regexp.MustCompile(`stderr log: (\S+)`).FindStringSubmatch(out)
		require.Len(t, outPath, 2)
		require.Len(t, errPath, 2)

		require.Eventually(t, func() bool {
			data, err := os.ReadFile(outPath[1])
			return err == nil && strings.Contains(string(data), "ajent: background process stopped")
		}, 5*time.Second, 10*time.Millisecond, "process must exit and be reaped")
		data, err := os.ReadFile(outPath[1])
		require.NoError(t, err)
		assert.Contains(t, string(data), "hi")
		assert.Contains(t, string(data), "(exit status 7)")
		edata, err := os.ReadFile(errPath[1])
		require.NoError(t, err)
		assert.Contains(t, string(edata), "boom")
		assert.Contains(t, string(edata), "ajent: background process stopped")
	})

	// a process killed by the reported pid gets the same stopped note
	t.Run("kill_reported_pid_leaves_stop_note", func(t *testing.T) {
		r := newBash(t, `{"command":"sleep 30","background":true}`)
		assert.False(t, r.res.IsError)
		out := textOf(r.res)
		pid := regexp.MustCompile(`pid (\d+)`).FindStringSubmatch(out)
		outPath := regexp.MustCompile(`stdout log: (\S+)`).FindStringSubmatch(out)
		require.Len(t, pid, 2)
		require.Len(t, outPath, 2)

		n, err := strconv.Atoi(pid[1])
		require.NoError(t, err)
		require.NoError(t, syscall.Kill(n, syscall.SIGTERM))

		assertEventuallyGone(t, n)
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(outPath[1])
			return err == nil && strings.Contains(string(data), "ajent: background process stopped")
		}, 5*time.Second, 10*time.Millisecond)
		data, err := os.ReadFile(outPath[1])
		require.NoError(t, err)
		assert.Contains(t, string(data), "(signal: terminated)")
	})

	// a long-running command must still be alive right after the call returns
	t.Run("outlives_the_call", func(t *testing.T) {
		r := newBash(t, `{"command":"sleep 30","background":true}`)
		assert.False(t, r.res.IsError)
		pid := regexp.MustCompile(`pid (\d+)`).FindStringSubmatch(textOf(r.res))
		require.Len(t, pid, 2)
		n, err := strconv.Atoi(pid[1])
		require.NoError(t, err)
		assert.NoError(t, syscall.Kill(n, 0), "process must still be running after the call returns")
		t.Cleanup(func() { _ = syscall.Kill(n, syscall.SIGKILL) })
	})
}

// TestBashBackgroundCloseKills proves agent shutdown (the tool's Close) kills
// every background command still running, including grandchildren.
func TestBashBackgroundCloseKills(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("-short mode")
	}

	dir := t.TempDir()
	// a TERM-trapping child proves the group kill sweeps beyond the leader
	cmd := fmt.Sprintf(`echo $$ > %s/pid.txt; sh -c 'trap "" TERM; sleep 300'`, dir)
	env := toolEnv{cwd: dir, tracker: NewTracker(), policy: PathPolicy{Cwd: dir}}
	tool := &bashTool{policy: env.policy, sessionID: "close-test"}
	c := agent.ToolCall{ID: "c", Name: "bash", Input: []byte(`{"command":` + strconv.Quote(cmd) + `,"background":true}`)}
	res, err := tool.Execute(t.Context(), c, &captureOutput{})
	require.NoError(t, err)
	assert.False(t, res.IsError)

	require.Eventually(t, func() bool {
		data, err := os.ReadFile(dir + "/pid.txt")
		return err == nil && len(strings.TrimSpace(string(data))) > 0
	}, 5*time.Second, 10*time.Millisecond, "the command must be running before Close")
	data, err := os.ReadFile(dir + "/pid.txt")
	require.NoError(t, err)
	pid, aerr := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, aerr)

	tool.Close()

	assertEventuallyGone(t, pid)
	require.Eventually(t, func() bool {
		err := syscall.Kill(-pid, 0)
		return err != nil && errors.Is(err, syscall.ESRCH)
	}, time.Second*2, time.Millisecond*30, "the whole process group must be gone")
}
