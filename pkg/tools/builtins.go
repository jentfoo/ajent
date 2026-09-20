package tools

import (
	"fmt"
	"os"

	"github.com/jentfoo/ajent/pkg/agent"
)

// Built-in tool names for the tools that only read.
const (
	ToolRead = "read"
	ToolGrep = "grep"
	ToolFind = "find"
	ToolLs   = "ls"
)

// ReadOnlyBuiltins names the built-in tools that only read. pkg/subagent keeps
// its own copy (readOnlyBuiltins) because it may not import this package.
var ReadOnlyBuiltins = []string{ToolRead, ToolGrep, ToolFind, ToolLs}

// ToolAskUser is the built-in question tool's name.
const ToolAskUser = "ask_user"

// Options configures the built-in tool set.
type Options struct {
	Cwd       string // base for relative paths; empty uses os.Getwd
	SessionID string // names the bash spill directory
	// ShellCommands lists common commands for the bash description's examples,
	// already filtered by the caller against PATH, deny rules and enabled tools.
	ShellCommands []string
	// Ask backs the ask_user tool. nil registers it in a state that reports no
	// UI is available, so a headless run never blocks on a question.
	Ask AskFunc
	// Vision reports the active model's image capability, consulted live by
	// read. nil disables read's image arm entirely.
	Vision func() bool
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
	reg.sessionID = opts.SessionID
	reg.Register(&readTool{policy: policy, tracker: tracker, vision: opts.Vision}, true)
	reg.Register(&writeTool{policy: policy, tracker: tracker}, true)
	reg.Register(&editTool{policy: policy, tracker: tracker}, true)
	reg.Register(&bashTool{policy: policy, sessionID: opts.SessionID, shellExamples: opts.ShellCommands}, true)
	reg.Register(&findTool{policy: policy, sessionID: opts.SessionID}, false)
	reg.Register(&grepTool{policy: policy, sessionID: opts.SessionID}, false)
	reg.Register(&lsTool{policy: policy, tracker: tracker, sessionID: opts.SessionID}, false)
	reg.Register(&askUserTool{ask: opts.Ask}, false)
	// a question changes nothing on disk, so it never needs approval
	reg.MarkReadOnly([]string{ToolAskUser})
	return reg, nil
}

var _ agent.ToolSet = (*Registry)(nil)
