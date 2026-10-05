package app

import (
	"io"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
)

// Exit codes a script can branch on. They are the same for text and json output.
const (
	ExitOK    = 0 // the turn completed and produced a final answer
	ExitUsage = 1 // bad flags, unknown model, or any setup failure before the turn
	ExitTurn  = 2 // the turn itself failed, was interrupted, or produced nothing
)

// Output shapes for a one-shot run.
const (
	OutputText = "text"
	OutputJSON = "json"
)

// ToolScope picks this invocation's permission posture: which tools a headless
// run offers the model, and the barrier mode its permission flag starts. A
// scope that names no mode leaves the configured default alone interactively,
// and runs auto+write headless (permissions.headlessMode overriding) where no
// dialog can settle a prompt and the model classifier is final.
type ToolScope uint8

const (
	ToolScopeDefault   ToolScope = iota // all built-ins; headless mode from permissions.headlessMode (default auto+write)
	ToolScopeAllowAll                   // everything incl. bash; starts at allow-all
	ToolScopeReadOnly                   // verifiably read-only tools only; starts at auto
	ToolScopeAuto                       // every built-in but the core writers; starts at auto
	ToolScopeAutoWrite                  // everything incl. bash; starts at auto+write
)

// barrierMode maps a scope onto the barrier mode its permission flag starts
// in, ok false when the scope names none and leaves the configured default
// alone. The zero Mode carries no meaning on false.
func (s ToolScope) barrierMode() (permit.Mode, bool) {
	switch s {
	case ToolScopeAllowAll:
		return permit.ModeAllowAll, true
	case ToolScopeReadOnly:
		// the offered set asks nothing, so auto never classifies unattended
		return permit.ModeAuto, true
	case ToolScopeAuto:
		return permit.ModeAuto, true
	case ToolScopeAutoWrite:
		return permit.ModeAutoWrite, true
	default:
		return 0, false
	}
}

// ResumeMode says what this run should do with saved sessions.
type ResumeMode int

const (
	ResumeNewSession  ResumeMode = iota // no flag: always a brand-new transcript
	ResumeContinue                      // --continue: auto-resume the most recent one
	ResumePick                          // --resume: picker over session roots, then resume its leaf
	ResumeID                            // --resume <id|name>: reopen that exact saved transcript directly
	ResumeSessionName                   // --session <name>: resume that name, creating it when new
)

// HeadlessOptions carries everything main resolved before deciding not to open a UI.
type HeadlessOptions struct {
	Set        *config.Set
	Reg        *llm.Registry
	Active     llm.Model
	SessMode   ResumeMode
	SessTarget string
	Warnings   []string

	Prompt     string
	Output     string // OutputText or OutputJSON
	Stats      bool
	Scope      ToolScope
	AllowTools []string
	DenyTools  []string

	Out      io.Writer
	Errw     io.Writer
	Provider func(llm.Model) (llm.Provider, error)
}

// RunOptions carries the parsed command line into app.Run.
type RunOptions struct {
	Model      string
	Render     string // ui.render override, "auto" means unset
	System     string // --system: replaces ajent's default prose guidance when non-empty
	Prompt     string
	Output     string // OutputText or OutputJSON
	Stats      bool
	Scope      ToolScope
	AllowTools []string
	DenyTools  []string
	Args       []string // positional arguments, joined into a bootstrap prompt

	SessMode   ResumeMode
	SessTarget string
}
