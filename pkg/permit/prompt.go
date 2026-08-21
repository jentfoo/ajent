package permit

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
)

// ErrDenied marks a dialog answer that explicitly refused (Esc) rather than the
// session having no UI, so the barrier reports a real user denial. main.go maps
// tui.ErrCancelled onto it.
var ErrDenied = errors.New("denied by user")

// Elision bounds for the dialog subject, matching tui's decision context budget
// so its own elide never has to cut twice.
const (
	decisionContextRows  = 16
	decisionContextChars = 480
)

// Prompter opens approval dialogs and asks for free-text reasons. main.go
// supplies a tui-backed implementation; nil means headless (no UI available).
type Prompter interface {
	Open(prompt, subject string, options []string) (Dialog, error)
	Reason(ctx context.Context, label string) (string, bool)
}

// Dialog is an open approval dialog. Wait blocks for the answer, Resolve settles
// it from the caller (the mode-change path), Close abandons it; first wins.
type Dialog interface {
	Wait(ctx context.Context) (int, error)
	Resolve(index int)
	Close()
}

// Noter injects a short user note into agent context at the next step boundary,
// used by "allow with note" so the model adapts without stopping the turn.
type Noter func(note string)

// dialogOption indexes one choice in an approval prompt. A plain command offers
// per-name session memory; a compound one replaces it with the strictly-greater
// broad grant, so exactly four options are shown either way.
const (
	optAllow         = iota // this call only
	optAllowNote            // allow and inject a steering note
	optAllowSession         // remember by command/tool name for the session (plain commands)
	optAllowCompound        // broad grant covering any compound command (compound commands)
	optDeny                 // refuse with an optional reason
)

// plainLabels are shown for a single-command call. There is nothing piped to cover,
// so only per-name memory applies and no broad grant is offered.
var plainLabels = []string{
	"Allow",
	"Allow with note",
	"Allow for session",
	"Deny",
}

// compoundLabels replace "allow for session" with the broader grant: a name can
// never remember a piped pipeline, so only the broad form is offered.
var compoundLabels = []string{
	"Allow",
	"Allow with note",
	"Allow compound for session",
	"Deny",
}

// buildOptions returns the dialog options for a call: per-name memory for a plain
// command, the broad grant in its place for a piped/redirected/substituted one.
func buildOptions(command string) []string {
	if !compound(command) {
		return slices.Clone(plainLabels)
	}
	return slices.Clone(compoundLabels)
}

// optionActions maps a dialog's display index back to the logical opt constant,
// matching buildOptions so Deny is always resolved as a denial regardless of where
// it sits in the rendered list.
func optionActions(command string) []int {
	if !compound(command) {
		return []int{optAllow, optAllowNote, optAllowSession, optDeny}
	}
	return []int{optAllow, optAllowNote, optAllowCompound, optDeny}
}

// allowSessionKey names what an "allow for session" remembers: the command name
// (bash:<head>) for shell commands, the tool name otherwise. Compound calls are
// never keyed this way; they take the broad grant instead.
func allowSessionKey(call agent.ToolCall) string {
	if call.Name != bashTool {
		return call.Name
	}
	s := scanCommand(bashCommand(call.Input))
	if len(s.Segments) == 0 {
		return bashTool
	}
	toks := segmentTokens(s.Segments[0])
	return "bash:" + stripPath(firstToken(toks))
}

// elideSubject bounds the dialog subject to decisionContextRows lines then
// decisionContextChars characters, so tui's own decision-context elision never has
// to cut twice. The first line always survives however long it is: the dialog wraps
// it, and a command must never be approved half-shown.
func elideSubject(s string) string {
	if s == "" {
		return ""
	}
	var out []string
	total := 0
	for _, ln := range strings.Split(s, "\n") {
		if len(out) > 0 && (len(out) >= decisionContextRows || total+len(ln) > decisionContextChars) {
			break
		}
		out = append(out, ln)
		total += len(ln)
	}
	return strings.Join(out, "\n")
}

// subjectFor renders what an approval dialog shows for a call: the shell command
// or the tool's raw arguments.
func subjectFor(call agent.ToolCall) string {
	if call.Name == "bash" {
		return bashCommand(call.Input)
	}
	return string(call.Input)
}

// dialogSubject renders what an approval shows for call: the injected preview (a
// write's content or an edit diff) when one is available, else the raw arguments.
func (b *Barrier) dialogSubject(call agent.ToolCall) string {
	if b.preview != nil {
		if s := b.preview(call); s != "" {
			return elideSubject(s)
		}
	}
	return elideSubject(subjectFor(call))
}
