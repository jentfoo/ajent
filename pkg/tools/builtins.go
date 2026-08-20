package tools

import (
	"fmt"
	"os"

	"github.com/jentfoo/ajent/pkg/agent"
)

// Options configures the built-in tool set.
type Options struct {
	Cwd       string // base for relative paths; empty uses os.Getwd
	SessionID string // names the bash spill directory
	// Ask backs the ask_user tool. nil registers it in a state that reports no
	// UI is available, so a headless run never blocks on a question.
	Ask AskFunc
}

// Builtins returns a registry holding read, write, edit and bash enabled plus
// find, grep, ls and ask_user registered disabled.
func Builtins(opts Options) (*Registry, error) {
	cwd := opts.Cwd
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("tools: cannot determine working directory: %w", err)
		}
	}

	tracker := NewTracker()
	policy := PathPolicy{Cwd: cwd}

	reg := New()
	reg.tracker = tracker
	reg.Register(&readTool{policy: policy, tracker: tracker}, true)
	reg.Register(&writeTool{policy: policy, tracker: tracker}, true)
	reg.Register(&editTool{policy: policy, tracker: tracker}, true)
	reg.Register(&bashTool{policy: policy, sessionID: opts.SessionID}, true)
	reg.Register(&findTool{policy: policy}, false)
	reg.Register(&grepTool{policy: policy}, false)
	reg.Register(&lsTool{policy: policy}, false)
	reg.Register(&askUserTool{ask: opts.Ask}, false)
	// a question changes nothing on disk, so it never needs approval
	reg.MarkReadOnly([]string{"ask_user"})
	return reg, nil
}

var _ agent.ToolSet = (*Registry)(nil)
