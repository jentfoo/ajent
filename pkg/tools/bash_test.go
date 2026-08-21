package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
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

func TestBashExitCodeReported(t *testing.T) {
	t.Parallel()

	r := newBash(t, `{"command":"exit 3"}`)
	assert.False(t, r.res.IsError)
	assert.Contains(t, textOf(r.res), "exit status 3")
}

func TestBashStreamsStdoutAndStderrInterleaved(t *testing.T) {
	t.Parallel()

	r := newBash(t, `{"command":"echo out; echo err >&2"}`)
	assert.False(t, r.res.IsError)
	assert.Contains(t, textOf(r.res), "out")
	assert.Contains(t, textOf(r.res), "err") // stderr captured for the model
}

func TestBashEmptyCommandRejected(t *testing.T) {
	t.Parallel()

	r := newBash(t, `{"command":"  "}`)
	assert.True(t, r.res.IsError)
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

func TestBashCancellationViaContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already cancelled: the command must not run to completion
	r := newBashCtx(ctx, t, `{"command":"echo should-not-appear"}`)
	assert.True(t, r.res.IsError) // cancellation is a clean stop marked as interrupted
	assert.Contains(t, textOf(r.res), "interrupted by user")
	assert.NotContains(t, textOf(r.res), "should-not-appear")
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

func TestBashElisionByLineBoundSpillsFileSuffix(t *testing.T) {
	t.Parallel()

	r := newBashWithLimit(t, Limit{Lines: 5}, `{"command":"seq 1 10000"}`)
	assert.False(t, r.res.IsError)
	out := textOf(r.res)
	assert.Contains(t, out, "... [truncated]")
}

func TestBashStripsANSIFromCapturedOutput(t *testing.T) {
	t.Parallel()

	r := newBash(t, `{"command":"printf '\\033[31mred\\033[0m plain'"}`)
	assert.False(t, r.res.IsError)
	out := textOf(r.res)
	assert.NotContains(t, out, "\x1b") // escapes stripped before the model sees it
	assert.Contains(t, out, "plain")
}

// TestBashPreservesParentPath guards against running bash as a login shell,
// which resets PATH to the system default and hides user dirs (~/.local/bin,
// Homebrew, nvm). A non-login shell must inherit our env verbatim.
func TestBashPreservesParentPath(t *testing.T) {
	t.Parallel()

	want := os.Getenv("PATH")
	r := newBash(t, `{"command":"printf %s \"$PATH\""}`)
	assert.False(t, r.res.IsError)
	assert.Equal(t, want, textOf(r.res))
}

func TestBashRespectsCwdOverride(t *testing.T) {
	t.Parallel()

	r := newBash(t, `{"command":"pwd","cwd":""}`)
	// cwd empty falls back to the policy cwd; assert pwd printed that dir
	assert.Contains(t, textOf(r.res), r.env.cwd)
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
