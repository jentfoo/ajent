package permit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/tools"
)

// noUIReason is the denial for a call that would prompt with nobody to ask.
const noUIReason = "permission required (no UI available)"

// maxClassifierArgs bounds a non-bash call's payload sent to auto classification,
// so a large write body never inflates the model request.
const maxClassifierArgs = 500

// bashTool is the built-in shell tool's name; its command feeds the classifier.
const bashTool = "bash"

// Barrier gates every tool call through static classification plus an optional
// approval dialog. It holds the live mode, session allows and any open dialogs,
// so a Shift+Tab cycle can re-evaluate what is on screen.
type Barrier struct {
	mu         sync.Mutex
	mode       Mode
	prompter   Prompter
	noter      Noter
	classifier Classifier
	notice     func(string)               // transient UI notices (auto-allow); nil = none
	dryRun     func(agent.ToolCall) error // registry-backed; nil means cannot predict
	ro         func(string) bool
	safe       func(agent.ToolCall) bool // config-declared safe commands; nil = none
	deny       func(agent.ToolCall) bool // config-declared denied commands; nil = none

	preview func(agent.ToolCall) string // enhanced dialog subject; nil = raw arguments

	allows          map[string]bool // session allows by allowSessionKey
	compoundAllowed bool            // broad grant covering any compound command
	open            []*pendingAsk   // live dialogs, re-evaluated on mode change
}

// pendingAsk tracks one open approval dialog so a mode change can resolve it and
// an answer can cancel its in-flight classification.
type pendingAsk struct {
	call   agent.ToolCall
	dlg    Dialog
	cancel context.CancelFunc // stops the concurrent classifier, nil when none started
}

// NewBarrier builds a barrier with read-only metadata lookup ro. It starts in
// allow-read; set prompter/classifier before use.
func NewBarrier(ro func(string) bool) *Barrier {
	return &Barrier{
		mode:   ModeAllowRead,
		allows: make(map[string]bool),
		ro:     ro,
	}
}

// SetPrompter installs the approval-dialog source; nil means headless.
func (b *Barrier) SetPrompter(p Prompter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prompter = p
}

// SetNoter installs note injection for "allow with note".
func (b *Barrier) SetNoter(n Noter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.noter = n
}

// SetClassifier installs the model classifier used in auto mode.
func (b *Barrier) SetClassifier(c Classifier) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.classifier = c
}

// SetNotice installs a callback for transient status notices such as an
// auto-allowed classification. main.go wires it to ui.Notify; nil silences them.
func (b *Barrier) SetNotice(n func(string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notice = n
}

// SetDryRun installs a registry-backed dry-run check for doomed calls. nil means
// no tool can predict failure, so nothing is skipped.
func (b *Barrier) SetDryRun(fn func(agent.ToolCall) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dryRun = fn
}

// SetPreview installs an optional per-call subject renderer (a write's content or
// an edit diff). It returns "" to fall back on the raw tool arguments.
func (b *Barrier) SetPreview(p func(agent.ToolCall) string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.preview = p
}

// SetSafeCommands installs config-declared safe commands: exact tool names or
// verbatim bash command lines that skip the approval prompt. write/edit can never
// be listed (they always prompt); an empty list clears any prior set.
func (b *Barrier) SetSafeCommands(cmds []string) {
	var fn func(agent.ToolCall) bool
	if len(cmds) > 0 {
		fn = func(call agent.ToolCall) bool { return SafeMatches(call, cmds) }
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.safe = fn
}

// SetDeniedCommands installs config-declared denied commands: exact tool names or
// verbatim bash command lines that are always refused without prompting, in every
// mode (allow-all and user-initiated included). An empty list clears any prior set.
func (b *Barrier) SetDeniedCommands(cmds []string) {
	var fn func(agent.ToolCall) bool
	if len(cmds) > 0 {
		fn = func(call agent.ToolCall) bool { return DenyMatches(call, cmds) }
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deny = fn
}

// Mode returns the current live mode.
func (b *Barrier) Mode() Mode {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mode
}

// SetMode swaps the live mode and re-evaluates any open dialog under it: a call
// the new mode would allow outright is resolved as allow. Never rewrites config.
func (b *Barrier) SetMode(m Mode) {
	b.mu.Lock()
	old := b.mode
	if old != m {
		b.mode = m
		b.resetSessionAllowsLocked() // each mode starts from its own default approvals
	}
	opens := slices.Clone(b.open)
	b.mu.Unlock()

	if old == m || len(opens) == 0 {
		return
	}
	for _, pa := range opens {
		if staticVerdict(m, context.Background(), pa.call, b.ro, b.dryRun, b.safe, b.deny).Action == tools.ActionAllow {
			pa.dlg.Resolve(int(optAllow))
		}
	}
}

// Cycle advances to the next mode in order and re-evaluates open dialogs.
func (b *Barrier) Cycle() Mode {
	b.mu.Lock()
	m := b.mode.Next()
	b.mode = m
	b.resetSessionAllowsLocked() // a new mode never inherits the previous gate's approvals
	opens := slices.Clone(b.open)
	b.mu.Unlock()

	for _, pa := range opens {
		if staticVerdict(m, context.Background(), pa.call, b.ro, b.dryRun, b.safe, b.deny).Action == tools.ActionAllow {
			pa.dlg.Resolve(int(optAllow))
		}
	}
	return m
}

// resetSessionAllowsLocked clears granted session memory so a mode change never
// carries one gate level's approvals into another. Caller holds the lock.
func (b *Barrier) resetSessionAllowsLocked() {
	clear(b.allows)
	b.compoundAllowed = false
}

// Guard returns the static gate: user-initiated and allow-all always permit;
// rejections deny with guidance; block-all asks even for reads. Never blocks.
func (b *Barrier) Guard() tools.Guard {
	return func(ctx context.Context, call agent.ToolCall) tools.Decision {
		return staticVerdict(b.Mode(), ctx, call, b.ro, b.dryRun, b.safe, b.deny)
	}
}

// Asker resolves a guard's ask into allow or deny. It consults session allows,
// opens an approval dialog, and in auto mode classifies bash concurrently.
func (b *Barrier) Asker() tools.Asker {
	return func(ctx context.Context, call agent.ToolCall, _ tools.Decision) tools.Decision {
		m := b.Mode()

		if _, ok := b.sessionAllowed(call); ok {
			return allowDecision()
		}

		prompter, _ := b.prompterSnapshot()
		if prompter == nil {
			return tools.Deny(noUIReason)
		}
		dlg, err := prompter.Open(promptText(m), b.dialogSubject(call), buildOptions(bashCommand(call.Input)))
		if err != nil || dlg == nil {
			return tools.Deny(noUIReason)
		}

		// auto mode classifies every prompted call concurrently with the dialog; a
		// readonly verdict resolves it open. A user answer cancels classification.
		var classifierCtx context.Context
		cancel := func() {}
		if m == ModeAuto && b.classifier != nil {
			classifierCtx, cancel = context.WithCancel(ctx)
		}

		b.mu.Lock()
		var cf context.CancelFunc
		if classifierCtx != nil {
			cf = cancel
		}
		pa := &pendingAsk{call: call, dlg: dlg, cancel: cf}
		b.open = append(b.open, pa)
		mNow := b.mode // same-lock capture so a concurrent SetMode/Cycle is ordered against registration
		b.mu.Unlock()

		// a Shift+Tab landed between Open and registration; re-evaluate under the new mode.
		if mNow != m && staticVerdict(mNow, ctx, call, b.ro, b.dryRun, b.safe, b.deny).Action == tools.ActionAllow {
			dlg.Resolve(int(optAllow))
		}

		if classifierCtx != nil && classifierCtx.Err() == nil {
			subject := classifierSubject(call)
			go func() {
				// a user answer cancels the context; skip both the resolve and its
				// auto-allowed notice so a denial is never reported as auto-allowed.
				if b.classifier.Classify(classifierCtx, subject) != ClassReadOnly ||
					classifierCtx.Err() != nil { // user answered while classifying
					return
				}
				dlg.Resolve(int(optAllow)) // first resolver wins; a keystroke beats this
				b.noticeAutoAllowed(subject)
			}()
		}

		idx, werr := dlg.Wait(ctx)
		cancel() // the user answered or gave up; stop any in-flight classification

		b.mu.Lock()
		b.open = bulk.SliceFilterInPlace(func(p *pendingAsk) bool { return p != pa }, b.open)
		b.mu.Unlock()

		if werr != nil {
			if errors.Is(werr, ErrDenied) {
				return tools.Deny("denied by user")
			}
			return tools.Deny(noUIReason)
		}
		return b.resolveChoice(ctx, call, idx)
	}
}

// resolveChoice maps a dialog answer to an allow/deny decision and applies its
// side effects (session memory, note injection). displayIdx is the position in
// the rendered option list, translated through optionActions so Deny on a plain
// command (where the compound grant was dropped) still refuses.
func (b *Barrier) resolveChoice(ctx context.Context, call agent.ToolCall, displayIdx int) tools.Decision {
	actions := optionActions(bashCommand(call.Input))
	if displayIdx >= 0 && displayIdx < len(actions) {
		switch actions[displayIdx] {
		case optAllow:
			return allowDecision()
		case optAllowNote:
			if prompter, _ := b.prompterSnapshot(); prompter != nil {
				if note, ok := prompter.Reason(ctx, "note for allowing"); ok && strings.TrimSpace(note) != "" {
					b.note(call, note)
				}
			}
			return allowDecision()
		case optAllowSession:
			if key, ok := b.sessionKey(call); ok && key != "" {
				b.mu.Lock()
				b.allows[key] = true
				b.mu.Unlock()
			}
			return allowDecision()
		case optAllowCompound:
			b.mu.Lock()
			b.compoundAllowed = true
			b.mu.Unlock()
			return allowDecision()
		}
	}
	// default and any out-of-range answer refuse, prompting for a reason when one is available.
	var reason string
	if prompter, _ := b.prompterSnapshot(); prompter != nil {
		if r, ok := prompter.Reason(ctx, "reason for denying"); ok {
			reason = strings.TrimSpace(r)
		}
	}
	if reason == "" {
		return tools.Deny("denied by user")
	}
	return tools.Deny("denied by user: " + reason)
}

// sessionKey returns the allow-session key for a call: command name for bash,
// tool name otherwise. Compound calls have no named grant.
func (b *Barrier) sessionKey(call agent.ToolCall) (string, bool) {
	if b.isCompound(call) {
		return "", false
	}
	return allowSessionKey(call), true
}

// isCompound reports whether a bash call carries pipes, redirects or substitution.
func (b *Barrier) isCompound(call agent.ToolCall) bool {
	return call.Name == bashTool && compound(bashCommand(call.Input))
}

// sessionAllowed checks the in-memory allow sets for a call. The returned string
// is empty when no grant matched; ok distinguishes a match from none.
func (b *Barrier) sessionAllowed(call agent.ToolCall) (string, bool) {
	if b.isCompound(call) {
		b.mu.Lock()
		defer b.mu.Unlock()
		return "", b.compoundAllowed
	}
	key := allowSessionKey(call)
	b.mu.Lock()
	defer b.mu.Unlock()
	return key, b.allows[key]
}

// note injects a steering note naming what was allowed and why.
func (b *Barrier) note(call agent.ToolCall, note string) {
	if b.noter == nil {
		return
	}
	name := call.Name
	if name == bashTool {
		s := scanCommand(bashCommand(call.Input))
		if len(s.Segments) > 0 {
			toks := segmentTokens(s.Segments[0])
			name = stripPath(firstToken(toks))
		}
	}
	b.noter("User allowed `" + name + "` with note: " + strings.TrimSpace(note))
}

// classifierSubject is what auto mode sends to classify call: the bash command,
// or the tool name plus its elided arguments for any other tool.
func classifierSubject(call agent.ToolCall) string {
	if call.Name == bashTool {
		return bashCommand(call.Input)
	}
	s := strings.TrimSpace(string(call.Input))
	if len(s) > maxClassifierArgs {
		s = s[:maxClassifierArgs] + "…"
	}
	return call.Name + " " + s
}

// noticeAutoAllowed tells the user a readonly verdict resolved an open dialog.
func (b *Barrier) noticeAutoAllowed(command string) {
	if n := b.noticeSnapshot(); n != nil {
		n("auto-allowed as read-only: " + elideSubject(command))
	}
}

// prompterSnapshot returns the installed prompter under read lock.
func (b *Barrier) prompterSnapshot() (Prompter, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.prompter, b.prompter != nil
}

// noticeSnapshot returns the installed notice callback under read lock.
func (b *Barrier) noticeSnapshot() func(string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.notice
}

// staticVerdict computes the guard's verdict for call under mode without blocking.
// Branch order is load-bearing: a configured denied command refuses first, before
// even allow-all or user-initiation. block-all asks even verifiably read-only
// calls, and a doomed edit runs so its natural error surfaces instead of prompting.
// A configured safe command overrides a prompt but never a reject, and still
// respects block-all (nothing auto-runs there).
func staticVerdict(m Mode, ctx context.Context, call agent.ToolCall, ro func(string) bool, dryRun func(agent.ToolCall) error, safe func(agent.ToolCall) bool, deny func(agent.ToolCall) bool) tools.Decision {
	// a config-declared denied command is refused outright in every mode.
	if deny != nil && deny(call) {
		return tools.Deny(deniedReason(call))
	}
	if m.allowsEverything() || tools.IsUserInitiated(ctx) {
		return tools.Allow(call)
	}
	switch Classify(call, ro) {
	case VerdictReject:
		return tools.Deny(rejectionReason(call))
	case VerdictPrompt:
		// a doomed edit runs so its natural error surfaces instead of prompting.
		if call.Name == "edit" && dryRun != nil && dryRun(call) != nil {
			return tools.Allow(call)
		}
		// a config-declared safe command skips the prompt; otherwise ask. A hard
		// reject above is never overridable, so sed -i stays refused.
		if safe == nil || !safe(call) {
			return askDecision()
		}
	}
	// VerdictAllow, or a Prompt matched by a configured safe command: auto-run
	// unless block-all prompts everything (reads included).
	if m == ModeBlockAll { // nothing auto-runs but ! lines under block-all
		return askDecision()
	}
	return tools.Allow(call)
}

// DenyMatches reports whether call is named by a configured denied command: an exact
// tool name for any non-bash tool (MCP/extension/built-in), or — for bash — the
// trimmed command line matched as a token-boundary prefix, so "git" covers every
// git invocation and "git stash" its subcommands. Unlike SafeMatches it may also
// name core writers; denying one is a legitimate safety gate.
func DenyMatches(call agent.ToolCall, cmds []string) bool {
	for _, e := range cmds {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if call.Name != bashTool && call.Name == e {
			return true
		}
		if call.Name == bashTool && commandHasPrefix(bashCommand(call.Input), e) {
			return true
		}
	}
	return false
}

// SafeMatches reports whether call is named by a configured safe command: an exact
// tool name for any non-bash tool (MCP/extension/built-in), or — for bash — the
// trimmed command line matched as a token-boundary prefix, so "git" covers every
// git invocation and "git status" its subcommands. write/edit can never be listed,
// so no config entry overrides a known writer.
func SafeMatches(call agent.ToolCall, cmds []string) bool {
	if _, isWrite := coreWriteTools[call.Name]; isWrite {
		return false
	}
	for _, e := range cmds {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if call.Name != bashTool && call.Name == e {
			return true
		}
		if call.Name == bashTool && commandHasPrefix(bashCommand(call.Input), e) {
			return true
		}
	}
	return false
}

// commandHasPrefix reports whether cmd starts with prefix at a token boundary, so
// "git status" matches any git-status subcommand but "cat" never matches "catalog".
func commandHasPrefix(cmd, prefix string) bool {
	cmd = strings.TrimSpace(cmd)
	if !strings.HasPrefix(cmd, prefix) {
		return false
	}
	rest := cmd[len(prefix):]
	if rest == "" {
		return true
	}
	switch rest[0] { // a boundary ends the matched token; a letter continues it
	case ' ', '\t', ';', '|', '&', '<', '>':
		return true
	default:
		return false
	}
}

// rejectionReason names what was refused and why, guiding the model.
func rejectionReason(call agent.ToolCall) string {
	if call.Name == bashTool {
		return "refused: in-place write (sed -i); use the edit tool instead"
	}
	return "refused: " + call.Name
}

// deniedReason names a config-denied command and why, guiding the model.
func deniedReason(call agent.ToolCall) string {
	if call.Name == bashTool {
		return "denied by configured deniedCommands"
	}
	return "refused: " + call.Name
}

// promptText is the dialog's question line for a mode.
func promptText(m Mode) string {
	if m == ModeBlockAll {
		return "block-all permits nothing without approval — run this?"
	}
	return "Allow this tool call?"
}

// allowDecision builds an allow decision (no reason needed).
func allowDecision() tools.Decision { return tools.Decision{Action: tools.ActionAllow} }

// askDecision asks for approval; the asker resolves it into allow or deny.
func askDecision() tools.Decision { return tools.Decision{Action: tools.ActionAsk} }
