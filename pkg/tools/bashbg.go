package tools

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
)

// ensureBg returns the root context every background command hangs off. A fresh
// root is genuinely needed: background runs must outlive every turn and call
// context, ending only at agent shutdown via Close.
func (t *bashTool) ensureBg() context.Context {
	t.bgOnce.Do(func() { t.bgCtx, t.bgStop = context.WithCancel(context.Background()) })
	return t.bgCtx
}

// Close kills every background command still running by cancelling its root
// context. Teardown reaches it through the registry, so no child outlives the
// process that started it.
func (t *bashTool) Close() {
	t.ensureBg()
	t.bgStop()
}

// executeBackground starts the command detached from the call context and
// returns its pid and log paths at once. root is the tool's background root
// (see ensureBg), never the caller's: the timeout ceiling does not apply and a
// cancelled turn must not kill the run. Only cancelling root (agent shutdown)
// stops it.
func (t *bashTool) executeBackground(root context.Context, cwd string, p bashParams) (agent.ToolResult, error) {
	outF, outPath, err := createSpill(t.sessionID, "bash-stdout")
	if err != nil {
		return agent.ErrorToolResult("bash: " + err.Error()), nil
	}
	errF, errPath, err := createSpill(t.sessionID, "bash-stderr")
	if err != nil {
		discardSpill(outF, outPath) // nothing will ever write the stdout log
		return agent.ErrorToolResult("bash: " + err.Error()), nil
	}

	// CommandContext over the background root, not the call context: the run
	// outlives the turn. root only cancels at agent shutdown, and Cancel does
	// the kill: the whole group, not just the leader, so grandchildren die too.
	cmd := exec.CommandContext(root, "bash", "-c", p.Command)
	cmd.Cancel = func() error {
		killGroup(cmd)
		return os.ErrProcessDone
	}
	cmd.Dir = cwd
	cmd.Env = bashEnv()
	cmd.Stdout = outF
	cmd.Stderr = errF
	ownProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		// chdir or exec failure: no live child will ever write these, drop both
		discardSpill(outF, outPath)
		discardSpill(errF, errPath)
		return agent.ErrorToolResult("bash: " + err.Error()), nil
	}

	pid := cmd.Process.Pid
	// os/exec writes straight into the two files (no copy goroutines), so Wait
	// returns as soon as bash exits and the handles are safe to reuse then
	go func() {
		waitErr := cmd.Wait()
		status := strings.TrimSpace(exitStatus(waitErr, cmd.ProcessState))
		if status == "" {
			status = "exit status 0"
		}
		note := "\najent: background process stopped (" + status + ")\n"
		_, _ = outF.WriteString(note)
		_, _ = errF.WriteString(note)
		_ = outF.Close()
		_ = errF.Close()
	}()

	// model-facing handoff: the pid to kill and the files to tail, so a later
	// call can inspect or stop the run
	pidStr := strconv.Itoa(pid)
	summary := "started in background, pid " + pidStr + "\n" +
		"stdout log: " + outPath + "\n" +
		"stderr log: " + errPath + "\n" +
		"stop it with: kill " + pidStr + "\n" +
		"review the logs with: tail -f " + outPath + " " + errPath
	return agent.ToolResult{Content: llmBlock(summary)}, nil
}
