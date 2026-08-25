package permit

import (
	"context"
	"errors"
	"fmt"
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
// used by "allow with note" and deny-with-reason so the model adapts without
// stopping the turn.
type Noter func(note string)

// dialogOption indexes one choice in an approval prompt. A command with a single
// identifiable head offers per-name session memory; only a complex compound (no
// reliable single command) replaces it with the strictly-greater broad grant, so
// exactly four options are shown either way.
const (
	optAllow         = iota // this call only
	optAllowNote            // allow and inject a steering note
	optAllowSession         // remember by command/tool name for the session
	optAllowCompound        // broad grant covering any compound command (complex compounds)
	optDeny                 // refuse with an optional reason
)

// plainLabels serve tool-name session memory for non-shell calls; a bash line with
// nameable heads gets its own per-command label instead.
var plainLabels = []string{
	"Allow",
	"Allow with note",
	"Allow for session",
	"Deny",
}

// compoundLabels replace per-name memory with the broad grant: a complex compound's
// command cannot be named, so only the broad form is offered.
var compoundLabels = []string{
	"Allow",
	"Allow with note",
	"Allow compound for session",
	"Deny",
}

// optionsFor returns the dialog's labels and their action mapping for command, kept
// index-aligned so resolveChoice maps a rendered choice back to its opt constant.
func optionsFor(command string) (labels []string, actions []int) {
	if names, ok := sessionNames(command); ok && len(names) > 0 {
		namedLabels := namedSessionLabel(names)
		return []string{"Allow", "Allow with note", namedLabels, "Deny"},
			[]int{optAllow, optAllowNote, optAllowSession, optDeny}
	}
	if compound(command) { // complex; only the broad grant reliably covers it
		return slices.Clone(compoundLabels),
			[]int{optAllow, optAllowNote, optAllowCompound, optDeny}
	}
	// no head to name and not compound (non-bash tool): plain per-name memory.
	return slices.Clone(plainLabels),
		[]int{optAllow, optAllowNote, optAllowSession, optDeny}
}

func buildOptions(command string) []string {
	labels, _ := optionsFor(command)
	return labels
}

// optionActions maps a dialog's display index back to the logical opt constant,
// matching optionsFor so Deny is always resolved as a denial regardless of where it
// sits in the rendered list.
func optionActions(command string) []int {
	_, actions := optionsFor(command)
	return actions
}

// namedSessionLabel renders the allow-for-session option for one or two commands:
// "Allow `ifconfig` for session" / "Allow `rm` and `mkdir` for session". The bash
// tool prefix is dropped since only shell commands reach here.
func namedSessionLabel(names []string) string {
	if len(names) == 2 {
		return fmt.Sprintf("Allow `%s` and `%s` for session", names[0], names[1])
	}
	return fmt.Sprintf("Allow `%s` for session", names[0])
}

// sessionNames returns the distinct non-readonly command names an "allow for
// session" would remember for a bash line. (nil,false) when no reliable name list
// exists — sub-shell/redirect or three or more commands — so only the broad grant applies.
func sessionNames(command string) ([]string, bool) {
	s := scanCommand(command)
	if !s.HasSplitOp && len(s.Segments) <= 1 { // a single simple command
		if s.HasUnsafeOp { // redirect/substitution: not a simple command
			return nil, false
		}
		h, ok := headOf(strings.TrimSpace(command))
		if !ok || h == "" {
			return nil, false
		}
		return []string{h}, true
	}
	heads, ok := compoundGoverningHeads(command)
	if !ok || len(heads) == 0 || len(heads) > 2 { // sub-shell or many commands: broad grant only
		return nil, false
	}
	return heads, true
}

// compoundGoverningHeads returns the distinct non-readonly segment heads of a bash
// line, or (nil,false) when sub-shells/redirects defeat reliable identification. A
// repeated head collapses so `git add x && git commit` names one command.
func compoundGoverningHeads(command string) ([]string, bool) {
	s := scanCommand(command)
	if s.HasUnsafeOp || len(s.Segments) == 0 {
		return nil, false
	}
	var heads []string
	for i, seg := range s.Segments {
		raw := ""
		if i < len(s.Raw) { // Segments and Raw stay index-aligned from pushSegment
			raw = s.Raw[i]
		}
		if segmentIsReadOnly(seg, raw) {
			continue // read-only segments never drive the prompt
		}
		h, ok := headOf(seg)
		if !ok || h == "" { // env-prefixed or unidentifiable head defeats analysis
			return nil, false
		}
		if slices.Contains(heads, h) {
			continue // repeated head collapses into one grant (git add && git commit)
		}
		heads = append(heads, h)
	}
	return heads, true
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
	h, ok := headOf(s.Segments[0])
	if !ok || h == "" { // env-prefixed/unnameable: never matches a grant
		return "bash:"
	}
	return "bash:" + h
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

// ClassifierSystem is auto-mode's prompt: classify one shell command as exactly one
// word by whether it changes any state.
const ClassifierSystem = `You classify a single shell command by its effect on the system. Reply with exactly one word and nothing else.

Categories:
- "readonly" — only reads or inspects data with no side effects: nothing is created, modified, or deleted, and no file, repo, process, network, or system state changes. Writing to stdout/stderr is fine.
- "write" — has any side effect: creates/modifies/deletes files, changes permissions or ownership, alters repo/process/system state, downloads, installs or runs software, redirects output to a file (>, >>), or reads from the network.
- "unsure" — only when you do not recognize the command name or genuinely cannot determine its effect.

Compound constructs — pipelines, command substitution $(...) or backticks, loops (for/while/until), and conditionals (if/case) — have no side effect of their own. Classify them by the commands they actually run: if every command inside only reads or inspects, the whole thing is "readonly"; if any one of them writes, it is "write". Examples: 'for f in *.md; do head -20 "$f"; done' and 'echo "=== $(basename "$f") ==="' are readonly; 'for f in *.tmp; do rm "$f"; done' and 'x=$(mktemp)' are write.

Use your general knowledge of Unix tools. The examples below are illustrative, NOT exhaustive — classify any unlisted command by what it actually does, not by whether it appears here.
- readonly examples: ls, cat, head, tail, grep, rg, find (no -exec/-delete), stat, file, df, du, wc, echo, printf, ps, env, uname, hostname, which, date, awk, jq, sed (without -i/--in-place and with no s///w or s///e flags in the script), tree, git status/log/diff/show.
- write examples: rm, mv, cp, touch, mkdir, rmdir, chmod, chown, ln, dd, truncate, tee, sed -i or sed --in-place; find -exec/-delete; git add/commit/checkout/reset/restore/push/pull/rebase/merge/stash; package installs (npm/pnpm/yarn/pip/apt/brew/cargo/go install); docker run/rm/kill, systemctl, mount, kill; curl/wget reading from or saving to the network.

Reserve "unsure" for unrecognized commands. Respond with ONLY the classification word.`

// MCPClassifierSystem is auto+mcp's prompt: classify one tool call by whether it
// changes any state. name, description and params are embedded so an unfamiliar
// server can be judged by what it declares.
func MCPClassifierSystem(name, description, params string) string {
	return fmt.Sprintf(`You classify a single tool invocation by its effect on the system. Reply with exactly one word and nothing else.

Categories:
- "readonly" — the call only reads or inspects: it changes no files, repo, process, network, remote service, permissions, configs, caches, credentials or any other state anywhere.
- "write" — has any side effect at all: mutates data, alters a remote system, sends commands with lasting effects, changes credentials or configuration.
- "unsure" — only when you cannot determine the tool's effect from its description and arguments.

A readonly verdict requires NO observable change to anything. Reading from the network is not readonly on its own (it can exfiltrate); it must also leave every system unchanged.

Tool under evaluation:
Name: %s
Description: %s
Parameters (JSON Schema):
%s`, name, strings.TrimSpace(description), params)
}
