package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// bashParams is the model-facing parameter block for bash.
type bashParams struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty" desc:"seconds before the whole process group is killed; default 120, max 600"`
	Cwd     string `json:"cwd,omitempty" desc:"working directory; defaults to the session cwd"`
}

const (
	defaultBashTimeout = 2 * time.Minute
	maxBashTimeout     = 10 * time.Minute
)

// ToolBash is the built-in shell tool's name; its command feeds the permission
// classifier.
const ToolBash = "bash"

// bashTool runs one non-login bash -c process per call. A fresh shell each time
// keeps cd and state from confusing later calls.
type bashTool struct {
	policy    PathPolicy
	sessionID string // names the spill directory for long output
	limit     Limit  // zero means BashOutput; overridable for tests
}

var _ agent.Tool = (*bashTool)(nil)

func (t *bashTool) Name() string { return ToolBash }

// Label returns a one-line summary of the command, bounded so a pathological
// one-liner cannot flood the header. The header wraps it; the status bar, which
// gets one row, truncates it to the width in force.
func (t *bashTool) Label(call agent.ToolCall) string {
	var p bashParams
	if err := decode(call.Input, &p); err != nil {
		return "bash"
	}
	cmd := strings.TrimSpace(strutil.FirstLine(p.Command))
	if cmd == "" {
		return "bash"
	}
	const maxLabel = 240
	r := []rune(cmd)
	if len(r) > maxLabel {
		return "bash: " + string(r[:maxLabel]) + "..."
	}
	return "bash: " + cmd
}

func (t *bashTool) Description() string {
	return "Execute a bash command in the session working directory. Returns stdout and stderr; output is truncated with the full log spilled to a file. Use timeout (seconds) to override the default."
}
func (t *bashTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: SchemaOf[bashParams]()} }
func (t *bashTool) Mode() agent.ExecutionMode {
	return agent.ModeSerial
}

// selfBounding: bash bounds and spills its own stream.
func (*bashTool) selfBounding() {}

// Execute streams command output to out while teeing a bounded head/tail copy,
// spilling the excess to disk so the model can read it back.
func (t *bashTool) Execute(ctx context.Context, call agent.ToolCall, out agent.Output) (agent.ToolResult, error) {
	out = ensureOutput(out)
	var p bashParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	if strings.TrimSpace(p.Command) == "" {
		return resultErr("bash needs a non-empty command"), nil
	}

	cwd := p.Cwd
	if cwd == "" {
		cwd = t.policy.Cwd
	} else if resolved, err := t.policy.Resolve(cwd); err != nil {
		return resultErr(err.Error()), nil
	} else {
		cwd = resolved
	}

	timeout := time.Duration(p.Timeout) * time.Second
	if timeout <= 0 {
		timeout = defaultBashTimeout
	}
	if timeout > maxBashTimeout {
		timeout = maxBashTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "bash", "-c", p.Command)
	cmd.Dir = cwd
	cmd.Env = bashEnv()
	// own process group: a timeout kill then sweeps grandchildren too
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		cmd.SysProcAttr.Setpgid = true
	}

	lim := t.limit
	if lim == (Limit{}) {
		lim = BashLimit()
	}
	var head bytes.Buffer // bounded kept prefix, spilled beyond the output limit
	spill := newSpiller(t.sessionID, "bash")
	defer func() { _ = spill.close() }()
	w := Writer(&head, lim, spill).(*boundedWriter)

	// hand os/exec our writers so its copy goroutines feed both streams into one
	// lock-protected sink; Wait joins those copies before returning, which avoids
	// racing descriptor cleanup against a manual pipe read. A grandchild that
	// outlives bash keeps the pipes open and stalls those copiers, so we sweep the
	// whole process group when done waiting instead of hanging.
	var writeMu sync.Mutex
	sink := &syncSink{mu: &writeMu, out: out, w: w}
	cmd.Stdout = sink
	cmd.Stderr = sink
	// backstop for a backgrounded child that holds the pipes open after bash
	// exits normally; without it os/exec would wait on those copiers forever.
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		if runCtx.Err() != nil { // cancelled before launch; the loop aborts this turn
			return resultErr(agent.InterruptedText), nil
		}
		return resultErr("bash: " + err.Error()), nil
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-runCtx.Done(): // timeout or cancel: CommandContext kills bash, then we sweep the group
		killGroup(cmd) // close grandchild-held pipes so Wait's copiers reach EOF and return
		waitErr = <-done
	}
	w.Flush()      // trailing partial line still held in the buffer
	killGroup(cmd) // sweep backgrounded survivors so they don't linger (safe post-Wait)
	var interrupted bool
	var statusText string
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		statusText = fmt.Sprintf("killed after %s timeout\n", timeout) + ownExitStatus(waitErr, cmd.ProcessState)
	case runCtx.Err() == context.Canceled:
		// parent cancelled (turn interrupt or Stager.Cancel): drop the SIGKILL exit
		// noise and mark the result so it reads as an interruption, not a failure.
		interrupted = true
		statusText = agent.InterruptedText + "\n"
	default:
		statusText = exitStatus(waitErr, cmd.ProcessState)
	}

	captured := normalizeToLF(head.String()) // model-visible text is LF-only
	truncated := w.Truncated() || overlongLine(captured)
	if truncated {
		// cap every kept line at MaxLineRunes like read/grep; the spill holds the
		// full stream, so capping is lossless.
		captured = capText(captured)
		if !w.Truncated() { // only an overlong line triggered this: no file yet,
			// write the complete stream so read can recover the uncapped tail
			_, _ = spill.Write(w.replay.Bytes())
		}
		lines, bytes := w.Total()
		note := truncationNote(Bounded{Shown: countLines(captured), Lines: lines, Bytes: bytes}, spill.path, "")
		captured += "\n" + note
	}

	return agent.ToolResult{Content: llmBlock(statusText + captured), IsError: interrupted}, nil
}

// syncSink writes sanitized output to both the live stream and the capture,
// serializing stdout/stderr under one lock so they stay in arrival order. One sink
// serves both streams, so stripping carries across chunk boundaries.
type syncSink struct {
	mu   *sync.Mutex
	out  agent.Output // live streamed sink, may discard
	w    io.Writer    // bounded capture (head + spill)
	ansi strutil.ANSIFilter
}

func (s *syncSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// under the lock: one strip position for both streams
	san := s.ansi.Strip(string(p))
	_, _ = io.WriteString(s.out, san)
	_, _ = io.WriteString(s.w, san)
	return len(p), nil
}

// bashEnv inherits the environment and forces non-interactive settings so no
// command waits on a pager or prompt.
func bashEnv() []string {
	env := os.Environ()
	set := func(k, v string) {
		for i := range env {
			if strings.HasPrefix(env[i], k+"=") {
				env[i] = k + "=" + v
				return
			}
		}
		env = append(env, k+"="+v)
	}
	set("AJENT", "1")
	set("PAGER", "cat")
	set("GIT_PAGER", "cat")
	set("TERM", "dumb")
	set("NO_COLOR", "1")
	set("GIT_TERMINAL_PROMPT", "0")
	set("DEBIAN_FRONTEND", "noninteractive")
	return env
}

// killGroup kills the whole process group led by cmd.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// exitStatus renders an exit-code summary for a completed command. state stays
// populated even when Wait returns ErrWaitDelay (orphaned grandchildren held our
// pipes open), so we can still report bash's real result rather than a spurious
// failure.
func exitStatus(err error, state *os.ProcessState) string {
	if err == nil || errors.Is(err, exec.ErrWaitDelay) && state != nil && state.Success() {
		return ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if sig, ok := deathSignal(ee); ok {
			return fmt.Sprintf("signal: %s\n", sig) // a signal death has no exit code
		}
		return fmt.Sprintf("exit status %d\n", ee.ExitCode())
	}
	if errors.Is(err, exec.ErrWaitDelay) { // real failure: command died but pipes lingered
		return "command failed: backgrounded child outlived the command\n"
	}
	return "command failed: " + err.Error() + "\n"
}

// deathSignal returns the signal that killed err's process, if a signal did
// rather than an exit code.
func deathSignal(err error) (syscall.Signal, bool) {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 0, false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return ws.Signal(), true
}

// ownExitStatus reports how a run we killed itself ended: the code it reached
// before our kill landed, or "" when that signal was the end of it. The timeout
// note already names that death, so an extra status there is noise.
func ownExitStatus(err error, state *os.ProcessState) string {
	if _, bySignal := deathSignal(err); bySignal {
		return ""
	}
	return exitStatus(err, state)
}
