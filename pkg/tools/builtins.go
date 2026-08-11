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
}

// Builtins returns a registry holding read, write, edit and bash enabled plus
// find, grep and ls registered disabled.
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
	reg.Register(&readTool{policy: policy, tracker: tracker}, true)
	reg.Register(&writeTool{policy: policy, tracker: tracker}, true)
	reg.Register(&editTool{policy: policy, tracker: tracker}, true)
	reg.Register(&bashTool{policy: policy, sessionID: opts.SessionID}, true)
	reg.Register(&findTool{policy: policy}, false)
	reg.Register(&grepTool{policy: policy}, false)
	reg.Register(&lsTool{policy: policy}, false)
	return reg, nil
}

var _ agent.ToolSet = (*Registry)(nil)
