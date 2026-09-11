package app

import (
	"io"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
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

// ToolScope is which tools a headless run offers the model. The scope is the
// gate: the barrier runs at allow-all, so nothing the model can see is refused.
type ToolScope uint8

const (
	ToolScopeDefault  ToolScope = iota // every built-in but bash
	ToolScopeAllowAll                  // every built-in, bash included
	ToolScopeReadOnly                  // verifiably read-only tools only
)

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
	Render     string // ui.render override; "auto" means unset
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
