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

const toolBash = "bash"

// bashTool runs one bash -lc process per call. A fresh shell each time keeps cd
// and state from confusing later calls.
type bashTool struct {
	policy    PathPolicy
	sessionID string // names the spill directory for long output
	limit     Limit  // zero means BashOutput; overridable for tests
}

var _ agent.Tool = (*bashTool)(nil)

func (t *bashTool) Name() string { return toolBash }

// Label returns a one-line summary of the command, truncated to fit a header.
func (t *bashTool) Label(call agent.ToolCall) string {
	var p bashParams
	if err := decode(call.Input, &p); err != nil {
		return "bash"
	}
	cmd := strings.TrimSpace(firstLine(p.Command))
	if cmd == "" {
		return "bash"
	}
	const maxLabel = 60
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

	cmd := exec.CommandContext(runCtx, "bash", "-lc", p.Command)
	cmd.Dir = cwd
	cmd.Env = bashEnv()
	// put the child in its own process group so killing it on timeout also kills
	// every grandchild instead of leaving orphans.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		cmd.SysProcAttr.Setpgid = true
	}

	lim := t.limit
	if lim == (Limit{}) {
		lim = BashOutput
	}
	var head bytes.Buffer // bounded kept prefix, spilled beyond the output limit
	spill := newSpiller(t.sessionID)
	defer func() { _ = spill.close() }()
	w := Writer(&head, lim, spill).(*boundedWriter)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return resultErr("bash: " + err.Error()), nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return resultErr("bash: " + err.Error()), nil
	}

	// stdout and stderr share one lock so the user sees them in arrival order and
	// both destinations get identical bytes.
	var writeMu sync.Mutex
	pump := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sanitized := stripANSI(string(buf[:n]))
				writeMu.Lock()
				_, _ = out.Write([]byte(sanitized))
				_, _ = w.Write([]byte(sanitized))
				writeMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(stdout) }()
	go func() { defer wg.Done(); pump(stderr) }()

	if err := cmd.Start(); err != nil {
		if runCtx.Err() != nil { // cancelled before launch; the loop aborts this turn
			return agent.ToolResult{}, nil
		}
		return resultErr("bash: " + err.Error()), nil
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	wg.Wait() // both pumps drained; flush any trailing partial line into head
	w.Flush()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		waitErr = <-done // CommandContext kills bash on expiry; Wait then returns
	}
	// a deadline or cancellation may have reaped only bash itself, leaving its
	// grandchild in the same process group behind — sweep that group explicitly.
	if runCtx.Err() != nil || ctx.Err() != nil {
		killGroup(cmd)
	}

	captured := head.String()
	if spill.path != "" {
		elided, _ := Elide(captured, lim)
		captured = elided + fmt.Sprintf("\n... output truncated; full log in @%s\n", spill.path)
	}
	statusText := exitStatus(waitErr)
	if runCtx.Err() == context.DeadlineExceeded {
		// the model must learn this was a timeout, not an ordinary failure
		statusText = fmt.Sprintf("killed after %s timeout\n", timeout) + statusText
	}

	return agent.ToolResult{Content: llmBlock(statusText + captured)}, nil
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

// exitStatus renders an exit-code summary for a completed command.
func exitStatus(err error) string {
	if err == nil {
		return ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exit status %d\n", ee.ExitCode())
	}
	return "command failed: " + err.Error() + "\n"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
