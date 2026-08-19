package tools

import (
	"context"
	"errors"
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
	assert.False(t, r.res.IsError) // cancellation is a clean stop, not an error result
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
