package permit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/tools"
)

// ErrDenied marks a dialog answer that explicitly refused (Esc) rather than the
// session having no UI, so the barrier reports a real user denial.
var ErrDenied = errors.New("denied by user")

// Elision bounds for the dialog subject, matching tui's decision context budget
// so its own elide never has to cut twice.
const (
	decisionContextRows  = 16
	decisionContextChars = 480
)

// Prompter opens approval dialogs and asks for free-text reasons. The host
// supplies a tui-backed implementation, nil meaning headless (no UI available).
type Prompter interface {
	// Hold waits out in-progress typing so the next Open never steals focus
	// from a draft, returning early once ctx ends. Hosts without a typing
	// signal return at once.
	Hold(ctx context.Context)
	Open(prompt, subject string, options []string) (Dialog, error)
	Reason(ctx context.Context, label string) (string, bool)
}

// Dialog is an open approval dialog. Wait blocks for the answer, Resolve settles
// it from the caller (the mode-change path), Close abandons it, first wins.
type Dialog interface {
	Wait(ctx context.Context) (int, error)
	Resolve(index int)
	Close()
}

// Noter injects a short user note into agent context at the next step boundary,
// used by "allow with note" and deny-with-reason so the model adapts without
// stopping the turn.
type Noter func(note string)

// dialogOption indexes one choice in an approval prompt. A bash line with
// identifiable heads offers per-name session memory, only a line no grant can
// cover (redirect/substitution, unnameable head) dropping that option.
const (
	optAllow        = iota // this call only
	optAllowNote           // allow and inject a steering note
	optAllowSession        // remember by command/tool name for the session
	optDeny                // refuse with an optional reason
)

// plainLabels serve tool-name session memory for non-shell calls, while a bash line with
// nameable heads gets its own per-command label instead.
var plainLabels = []string{
	"Allow",
	"Allow with note",
	"Allow for session",
	"Deny",
}

// bareLabels serve a line no session grant can cover (redirect, substitution,
// unnameable head): allowing it never generalizes, so no memory option is offered.
var bareLabels = []string{
	"Allow",
	"Allow with note",
	"Deny",
}

// optionsFor returns the dialog's labels and their action mapping for command, kept
// index-aligned so resolveChoice maps a rendered choice back to its opt constant.
func optionsFor(command string) (labels []string, actions []int) {
	if names, ok := sessionNames(command); ok && len(names) > 0 {
		return []string{"Allow", "Allow with note", namedSessionLabel(names), "Deny"},
			[]int{optAllow, optAllowNote, optAllowSession, optDeny}
	}
	if compound(command) { // redirect/substitution: no grant could cover a future line
		return slices.Clone(bareLabels),
			[]int{optAllow, optAllowNote, optDeny}
	}
	// no head to name and not compound (non-bash tool): plain per-name memory.
	return slices.Clone(plainLabels),
		[]int{optAllow, optAllowNote, optAllowSession, optDeny}
}

func buildOptions(command string) []string {
	labels, _ := optionsFor(command)
	return labels
}

// optionActions maps a dialog's display index back to the logical opt constant, matching
// optionsFor so Deny is always resolved as a denial regardless of where it sits in the rendered list.
func optionActions(command string) []int {
	_, actions := optionsFor(command)
	return actions
}

// namedSessionLabel renders the allow-for-session option over the named commands:
// "Allow `ifconfig` for session" / "Allow `rm` and `mkdir` for session", lists past
// three eliding into "and N more". The bash tool prefix is dropped since only
// shell commands reach here.
func namedSessionLabel(names []string) string {
	const maxShown = 3
	shown, more := names, 0
	if len(names) > maxShown {
		shown, more = names[:maxShown], len(names)-maxShown
	}
	var b strings.Builder
	b.WriteString("Allow ")
	for i, n := range shown { // 1-3 names: `a`, `a` and `b`, `a`, `b` and `c`
		switch {
		case i == 0:
		case i == len(shown)-1:
			b.WriteString(" and ")
		default:
			b.WriteString(", ")
		}
		b.WriteString("`")
		b.WriteString(n)
		b.WriteString("`")
	}
	if more > 0 { // parenthetical keeps the single "and" of the join unambiguous
		fmt.Fprintf(&b, " (+%d more)", more)
	}
	b.WriteString(" for session")
	return b.String()
}

// sessionNames returns the distinct non-readonly command names an "allow for
// session" would remember for a bash line, read-only segments never counting.
// (nil,false) when no reliable name list exists (a sub-shell/redirect or an
// unnameable head), so the dialog offers no session memory at all.
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
	if !ok || len(heads) == 0 { // sub-shell or nothing nameable: no session memory
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
		var raw string
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
// (bash:<head>) for shell commands, the tool name otherwise. The head comes from
// the raw command, the same derivation sessionNames uses when the grant is
// written, so a quoted head ('git' log) looks up the key it was stored under.
// Lines no grant can cover are never keyed this way, offering no session memory
// instead.
func allowSessionKey(call agent.ToolCall) string {
	if call.Name != tools.ToolBash {
		return call.Name
	}
	cmd := bashCommand(call.Input)
	s := scanCommand(cmd)
	if len(s.Segments) == 0 {
		return tools.ToolBash
	}
	h, ok := headOf(strings.TrimSpace(cmd))
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
	var total int
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
	if call.Name == tools.ToolBash {
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

// ClassifierSystem is auto-mode's prompt: decide one shell command in exactly one
// word, allowing only what changes no state at all.
const ClassifierSystem = `You decide whether a single shell command may run unattended in a coding session. Reply with exactly one word and nothing else.

Categories:
- "allow": only reads or inspects data with no side effects. Nothing is created, modified, or deleted, and no file, repo, process, network, or system state changes. Writing to stdout/stderr is fine.
- "deny": has any side effect. It creates/modifies/deletes files, changes permissions or ownership, alters repo/process/system state, downloads, installs or runs software, redirects output to a file (>, >>), or reads from the network.
- "unsure": only when you do not recognize the command name or genuinely cannot determine its effect.

Compound constructs (pipelines, command substitution $(...) or backticks, loops for/while/until, and conditionals if/case) have no side effect of their own. Judge them by the commands they actually run: if every command inside only reads or inspects, the whole thing is "allow"; if any one writes, it is "deny". Examples: 'for f in *.md; do head -20 "$f"; done' and 'echo "=== $(basename "$f") ==="' are allow; 'for f in *.tmp; do rm "$f"; done' and 'x=$(mktemp)' are deny.

Use your general knowledge of Unix tools. The examples below are illustrative, NOT exhaustive. Judge any unlisted command by what it actually does, not by whether it appears here.
- allow examples: ls, cat, head, tail, grep, rg, find (no -exec/-delete), stat, file, df, du, wc, echo, printf, ps, env, uname, hostname, which, date, awk, jq, sed (without -i/--in-place and with no s///w or s///e flags in the script), tree, git status/log/diff/show.
- deny examples: rm, mv, cp, touch, mkdir, rmdir, chmod, chown, ln, dd, truncate, tee, sed -i or sed --in-place; find -exec/-delete; git add/commit/checkout/reset/restore/push/pull/rebase/merge/stash; package installs (npm/pnpm/yarn/pip/apt/brew/cargo/go install); docker run/rm/kill, systemctl, mount, kill; curl/wget reading from or saving to the network.

Reserve "unsure" for unrecognized commands. Respond with ONLY the one word.`

// WorkspaceClassifierSystem is auto+write's prompt: judge one shell command by
// whether it is permissible, given cwd and tmp as the only writable roots.
func WorkspaceClassifierSystem(cwd, tmp string) string {
	return fmt.Sprintf(`You decide whether a single shell command may run unattended in a coding session. Reply with exactly one word and nothing else.

These two directories are the workspace, and are the ONLY places anything may be created, modified or deleted:
- %s
- %s

Categories:
- "allow": the command only reads or inspects anywhere, or it changes things inside the workspace. Reading and writing stdout/stderr is always fine; so is writing within the current working directory or %[2]s.
- "deny": the command writes outside the workspace, changes the system, destroys work, or runs something you cannot account for.
- "unsure": only when you do not recognize the command name or genuinely cannot determine its effect.

Reading never needs approval: inspecting a file, directory, process or path anywhere (inside or outside the workspace) is always "allow"; the sole exception is security-sensitive credentials and secrets. Only mutations are confined to the two roots above.

Allowed inside the workspace: creating and overwriting files; python, perl, ruby, awk or sed invocations that rewrite or generate files there; writing through cat/tee or the > and >> redirects; mkdir, rmdir, touch, mv and cp; removing individual named files; running the project's own build, test and lint commands. Safe git commands like add, commit, stash with restore in the repository also allowed.

Network requests need discretion. Common use for retrieving docs is safe, many read operations may be safe. But deny any which could be attempting to exfiltrate data or do a sensitive action.

Always deny, whatever else the command does:
- destroying work in bulk: rm -rf on a directory, rm with a broad wildcard, truncating or emptying many files
- Potentially dangerous git commands, for example: git clean -fdx, git reset --hard, git rebase, git push, or any history rewrite
- changing the system or its software: sudo, su, apt, brew, yum, systemctl, service, mount, launchctl, chmod, chown, kill, or a global/system package install (npm -g, pip install, apt install, dpkg, yum)
- running software you cannot account for: an executable downloaded or generated by this same command line, a script piped into a shell (curl ... | sh, base64 -d | sh), eval of a constructed string, or anything deliberately obscured
- reading or copying credentials, keys, tokens or environment secrets
- writing inside a repository's metadata directory (.git, .hg, .svn), since a hook or config alias there runs on the next command

Compound constructs (pipelines, command substitution $(...) or backticks, loops for/while/until, and conditionals if/case) have no effect of their own. Judge them by what the commands actually run, in the same read-first order: every part that only reads is "allow" wherever its paths are; a single part that writes outside the workspace makes the whole "deny". A cd into the workspace does not make a later absolute path safe.

Use your general knowledge of Unix tools; the lists above are illustrative, NOT exhaustive. When a path is relative, assume it resolves inside %s. When you cannot tell whether or where a command writes, answer "unsure" rather than "allow". Respond with ONLY the one word.`, cwd, tmp, cwd)
}

// MCPClassifierSystem is the model classifier's prompt for a tool call: decide
// whether it changes any state. name, description and params are embedded so an unfamiliar
// server can be judged by what it declares.
func MCPClassifierSystem(name, description, params string) string {
	return fmt.Sprintf(`You decide whether a single tool invocation may run unattended in a coding session. Reply with exactly one word and nothing else.

Categories:
- "allow": the call only reads or inspects. It changes no files, repo, process, network, remote service, permissions, configs, caches, credentials or any other state anywhere.
- "deny": has any side effect at all. It mutates data, alters a remote system, sends commands with lasting effects, changes credentials or configuration.
- "unsure": only when you cannot determine the tool's effect from its description and arguments.

An allow verdict requires NO observable change to anything. Reading from the network is not allowed on its own (it can exfiltrate); it must also leave every system unchanged.

Tool under evaluation:
Name: %s
Description: %s
Parameters (JSON Schema):
%s`, name, strings.TrimSpace(description), params)
}
