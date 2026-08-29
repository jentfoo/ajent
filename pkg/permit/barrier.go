package permit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/strutil"
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
// approval dialog, holding the live mode and any open dialogs.
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
	scope      writeScope                // auto+write's writable roots; zero allows nothing

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
	auto   bool               // an allow verdict resolved this dialog (auto mode)
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
// bash command lines that skip the approval prompt. write/edit can never be
// listed (they always prompt); an empty list clears any prior set.
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
// bash command lines that are always refused without prompting, in every
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

// SetWriteRoots installs the directories auto+write may write to without a
// prompt: cwd plus any extra roots (the temp dir). Unset means every write
// prompts, as in every other mode.
func (b *Barrier) SetWriteRoots(cwd string, extra ...string) {
	s := newWriteScope(cwd, extra...)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.scope = s
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
	g := b.gateFor(m)
	for _, pa := range opens {
		if g.staticVerdict(context.Background(), pa.call).Action == tools.ActionAllow {
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

	g := b.gateFor(m)
	for _, pa := range opens {
		if g.staticVerdict(context.Background(), pa.call).Action == tools.ActionAllow {
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
		return b.gateNow().staticVerdict(ctx, call)
	}
}

// Asker resolves a guard's ask into allow or deny. It consults session allows,
// opens an approval dialog, and in auto mode classifies bash concurrently.
func (b *Barrier) Asker() tools.Asker {
	return func(ctx context.Context, call agent.ToolCall, _ tools.Decision) tools.Decision {
		g := b.gateNow()
		m := g.mode

		if _, ok := b.sessionAllowed(call); ok {
			return allowDecision()
		}

		prompter, _ := b.prompterSnapshot()
		if prompter == nil {
			return tools.Deny(noUIReason)
		}
		dlg, err := prompter.Open(promptText(m, call.Name), b.dialogSubject(call), buildOptions(bashCommand(call.Input)))
		if err != nil || dlg == nil {
			return tools.Deny(noUIReason)
		}

		// the auto modes classify every prompted call concurrently with the dialog; an
		// allow verdict resolves it open. A user answer cancels classification.
		var classifierCtx context.Context
		cancel := func() {}
		if b.classifyCall(m, call.Name) {
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
		if mNow != m && b.gateFor(mNow).staticVerdict(ctx, call).Action == tools.ActionAllow {
			dlg.Resolve(int(optAllow))
		}

		if classifierCtx != nil && classifierCtx.Err() == nil {
			subject := classifySubject(m, call)
			go func() {
				// a user answer cancels the context; skip both the resolve and its
				// auto-allowed report so a denial is never claimed as auto-allowed.
				if b.classifier.Classify(classifierCtx, subject) != ClassAllow ||
					classifierCtx.Err() != nil { // user answered while classifying
					return
				}
				b.mu.Lock()
				pa.auto = true // set before Resolve so Wait's return already sees it
				b.mu.Unlock()
				dlg.Resolve(int(optAllow)) // first resolver wins; a keystroke beats this
			}()
		}

		idx, werr := dlg.Wait(ctx)
		cancel() // the user answered or gave up; stop any in-flight classification

		b.mu.Lock()
		auto := pa.auto
		b.open = bulk.SliceFilterInPlace(func(p *pendingAsk) bool { return p != pa }, b.open)
		b.mu.Unlock()

		if werr != nil {
			if errors.Is(werr, ErrDenied) {
				return tools.Deny("denied by user")
			}
			return tools.Deny(noUIReason)
		}
		return b.resolveChoice(ctx, call, idx, auto)
	}
}

// resolveChoice maps a dialog answer to an allow/deny decision and applies its
// side effects (session memory, note injection). displayIdx is the position in
// the rendered option list, translated through optionActions so Deny on a plain
// command (where the compound grant was dropped) still refuses.
func (b *Barrier) resolveChoice(ctx context.Context, call agent.ToolCall, displayIdx int, auto bool) tools.Decision {
	actions := optionActions(bashCommand(call.Input))
	if displayIdx >= 0 && displayIdx < len(actions) {
		switch actions[displayIdx] {
		case optAllow:
			b.resolveNotice("once", auto)
			return allowDecision()
		case optAllowNote:
			if prompter, _ := b.prompterSnapshot(); prompter != nil {
				if note, ok := prompter.Reason(ctx, "note for allowing"); ok && strings.TrimSpace(note) != "" {
					b.noteAllowed(call, note)
				}
			}
			b.resolveNotice("once", auto)
			return allowDecision()
		case optAllowSession:
			if keys, ok := b.allowSessionKeys(call); ok && len(keys) > 0 {
				b.mu.Lock()
				for _, k := range keys {
					b.allows[k] = true
				}
				b.mu.Unlock()
			}
			b.resolveNotice("session", auto)
			return allowDecision()
		case optAllowCompound:
			b.mu.Lock()
			b.compoundAllowed = true
			b.mu.Unlock()
			b.resolveNotice("session", false) // only the named grant is auto-resolved, never the broad one
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
	b.noteDenied(call, reason) // a typed denial reason reaches the model as a user message
	if reason == "" {
		return tools.Deny("denied by user")
	}
	return tools.Deny("denied by user: " + reason)
}

// allowSessionKeys returns the keys an "allow for session" remembers: the tool name,
// or one "bash:<name>" per identifiable non-readonly command. (nil,false) when only
// the broad grant applies.
func (b *Barrier) allowSessionKeys(call agent.ToolCall) ([]string, bool) {
	if call.Name != bashTool {
		return []string{call.Name}, true // tool name for non-bash
	}
	names, ok := sessionNames(bashCommand(call.Input))
	if !ok || len(names) == 0 {
		return nil, false // complex compound: no named grant to remember
	}
	keys := make([]string, 0, len(names))
	for _, n := range names {
		keys = append(keys, "bash:"+n)
	}
	return keys, true
}

// sessionAllowed checks the in-memory allow sets for a call. The returned string
// is empty when no grant matched; ok distinguishes a match from none. A named grant
// covers a plain command and any compound whose non-readonly heads are all granted;
// only a complex (unidentifiable) compound falls back to the broad grant.
func (b *Barrier) sessionAllowed(call agent.ToolCall) (string, bool) {
	cmd := bashCommand(call.Input)
	if call.Name != bashTool || !compound(cmd) { // plain command or non-bash tool
		key := allowSessionKey(call)
		b.mu.Lock()
		defer b.mu.Unlock()
		return key, b.allows[key]
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	heads, ok := compoundGoverningHeads(cmd)
	if !ok || len(heads) == 0 {
		return "", b.compoundAllowed // unidentifiable: only the broad grant covers it
	}
	for _, h := range heads {
		if !b.allows["bash:"+h] {
			// a named head missing; the broad grant may still cover this compound.
			return "", b.compoundAllowed
		}
	}
	return "session", true // every governing command is granted by name
}

// noteAllowed injects a steering note naming what was allowed and why.
func (b *Barrier) noteAllowed(call agent.ToolCall, note string) {
	if b.noter == nil || strings.TrimSpace(note) == "" {
		return
	}
	b.noter("Allowed with note: " + strings.TrimSpace(note))
}

// noteDenied injects a denial reason as a user message so the model sees why a
// call was refused, mirroring allow-with-note; an empty reason injects nothing.
func (b *Barrier) noteDenied(call agent.ToolCall, reason string) {
	if b.noter == nil || strings.TrimSpace(reason) == "" {
		return
	}
	b.noter("Denied with note: " + strings.TrimSpace(reason))
}

// classifyCall reports whether mode sends this call type to the model classifier:
// auto judges unverifiable bash commands plus MCP/extension tool calls (judged
// with their metadata); auto+write adds write confinement. A core writer never
// goes to the model — Classify already decided it, and a stray allow verdict would
// auto-allow it. Every other built-in is resolved by Classify and so never reaches
// this. A nil classifier never starts one.
func (b *Barrier) classifyCall(m Mode, name string) bool {
	if b.classifier == nil {
		return false
	}
	switch m {
	case ModeAuto, ModeAutoWrite:
		if name == bashTool {
			return true
		}
		_, isWrite := coreWriteTools[name]
		return !isWrite // MCP and other extension tools, judged with their metadata
	default:
		return false
	}
}

// classifySubject is what auto/auto+write sends to classify call: the bash
// command, or the tool name plus its elided arguments for any other (MCP) tool.
// AllowWrite selects the workspace rule set, and only ever for a shell command.
func classifySubject(m Mode, call agent.ToolCall) Subject {
	if call.Name == bashTool {
		// the declared cwd rebases every relative path, so the model must see it
		return Subject{
			Name:       bashTool,
			Args:       bashCommand(call.Input),
			Cwd:        bashCwd(call.Input),
			AllowWrite: m.allowsWrites(),
		}
	}
	s := strings.TrimSpace(string(call.Input))
	return Subject{Name: call.Name, Args: strutil.Clip(s, maxClassifierArgs)}
}

// resolveNotice reports how an approved call was granted, replacing the dialog's
// generic prompt echo with a descriptive outcome line.
func (b *Barrier) resolveNotice(scope string, auto bool) {
	n := b.noticeSnapshot()
	if n == nil {
		return
	}
	switch {
	case auto:
		n("Tool auto allowed")
	case scope == "session":
		n("Tool allowed for session")
	default:
		n("Tool call allowed this time")
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

// gate is the barrier's decision state, snapshotted under the lock so a verdict
// never races a Set* installer.
type gate struct {
	mode   Mode
	ro     func(string) bool
	dryRun func(agent.ToolCall) error
	safe   func(agent.ToolCall) bool
	deny   func(agent.ToolCall) bool
	scope  writeScope
}

// gateFor snapshots the decision state alongside mode.
func (b *Barrier) gateFor(m Mode) gate {
	b.mu.Lock()
	defer b.mu.Unlock()
	return gate{mode: m, ro: b.ro, dryRun: b.dryRun, safe: b.safe, deny: b.deny, scope: b.scope}
}

// gateNow snapshots the decision state with the live mode.
func (b *Barrier) gateNow() gate {
	b.mu.Lock()
	defer b.mu.Unlock()
	return gate{mode: b.mode, ro: b.ro, dryRun: b.dryRun, safe: b.safe, deny: b.deny, scope: b.scope}
}

// staticVerdict computes the guard's verdict for call under mode without blocking.
// Branch order is load-bearing: a configured denied command refuses first, before
// even allow-all or user-initiation. auto+write's in-scope write runs next, so a
// config denial still outranks it. block-all asks even verifiably read-only
// calls, and a doomed edit runs so its natural error surfaces instead of prompting.
// A configured safe command overrides a prompt but never a reject, and still
// respects block-all (nothing auto-runs there).
func (g gate) staticVerdict(ctx context.Context, call agent.ToolCall) tools.Decision {
	m := g.mode
	// a user-issued ! line runs regardless of config denial or mode
	if tools.IsUserInitiated(ctx) {
		return tools.Allow(call)
	}
	// a config-declared denied command is refused outright in every mode.
	if g.deny != nil && g.deny(call) {
		return tools.Deny(deniedReason(call))
	}
	if m.allowsEverything() {
		return tools.Allow(call)
	}
	// workspace-confined writes run before Classify, which always prompts a core writer
	if m.allowsWrites() && g.scope.allows(call) {
		return tools.Allow(call)
	}
	switch Classify(call, g.ro) {
	case VerdictReject:
		return tools.Deny(rejectionReason(call))
	case VerdictPrompt:
		// a doomed edit runs so its natural error surfaces instead of prompting.
		if call.Name == "edit" && g.dryRun != nil && g.dryRun(call) != nil {
			return tools.Allow(call)
		}
		// a config-declared safe command skips the prompt; otherwise ask. A hard
		// reject above is never overridable, so sed -i stays refused.
		if g.safe == nil || !g.safe(call) {
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
// tool name for any non-bash tool (MCP/extension/built-in), or, for bash, the
// trimmed command line matched as a token-boundary prefix, so "git" covers every
// git invocation and "git stash" its subcommands. A compound line is refused when
// any of its components matches, so wrapping in `cd ... &&` never escapes the gate.
// Unlike SafeMatches it may also name core writers; denying one is a legitimate safety gate.
func DenyMatches(call agent.ToolCall, cmds []string) bool {
	for _, e := range cmds {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if call.Name != bashTool && toolNameCovered(call.Name, e) {
			return true
		}
	}
	if call.Name == bashTool {
		for _, seg := range scanCommand(bashCommand(call.Input)).Segments {
			if entryCovered(seg, cmds) {
				return true
			}
		}
	}
	return false
}

// SafeMatches reports whether call is named by a configured safe command: an exact
// tool name for any non-bash tool (MCP/extension/built-in), or, for bash, the
// trimmed command line matched as a token-boundary prefix, so "git" covers every
// git invocation and "git status" its subcommands. A compound line matches only when
// every component is either a listed entry or verifiably read-only (mirroring
// allSegmentsReadOnly's all-or-nothing gate), so an appended write never rides in.
// write/edit can never be listed, so no config entry overrides a known writer.
func SafeMatches(call agent.ToolCall, cmds []string) bool {
	if _, isWrite := coreWriteTools[call.Name]; isWrite {
		return false
	}
	for _, e := range cmds {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if call.Name != bashTool && toolNameCovered(call.Name, e) {
			return true
		}
	}
	if call.Name == bashTool {
		return safeBashLine(bashCommand(call.Input), cmds)
	}
	return false
}

// safeBashLine reports whether a bash line is covered by configured entries. A single
// command matches on the token-boundary prefix of its trimmed text; a compound (control
// operators or substitution) instead requires every component to be either a listed entry
// or verifiably read-only, so "make lint" can never smuggle in an appended write.
func safeBashLine(cmd string, cmds []string) bool {
	s := scanCommand(cmd)
	if s.HasUnsafeOp { // > ` $( <( defeat analysis; fail to the prompt path
		return false
	}
	if !s.HasSplitOp && len(s.Segments) <= 1 {
		return entryCovered(strings.TrimSpace(cmd), cmds)
	}
	for i, seg := range s.Segments {
		rw := ""
		if i < len(s.Raw) { // Segments and Raw stay index-aligned from pushSegment
			rw = s.Raw[i]
		}
		if entryCovered(seg, cmds) || segmentIsReadOnly(seg, rw) {
			continue
		}
		return false
	}
	return true
}

// entryCovered reports whether line starts with a configured command at a token boundary.
func entryCovered(line string, cmds []string) bool {
	for _, e := range cmds {
		e = strings.TrimSpace(e)
		if commandHasPrefix(line, e) {
			return true
		}
	}
	return false
}

// toolNameCovered reports whether a non-bash tool name is covered by a configured
// entry: the exact tool name, or (when the entry names an MCP server namespace,
// tools are registered server__tool) every tool that server exposes. An extension
// whose own name carries __ still matches exactly.
func toolNameCovered(name, e string) bool {
	if name == e {
		return true
	}
	return strings.HasPrefix(name, e+"__")
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
		return "denied by configuration, ask user to run if necessary"
	}
	return "refused: " + call.Name
}

// promptText is the dialog's question line for a mode and tool.
func promptText(m Mode, name string) string {
	if m == ModeBlockAll { // block-all genuinely prompts everything
		return "block-all permits nothing without approval — run this?"
	}
	return fmt.Sprintf("Allow `%s` tool call?", name)
}

// allowDecision builds an allow decision (no reason needed).
func allowDecision() tools.Decision { return tools.Decision{Action: tools.ActionAllow} }

// askDecision asks for approval; the asker resolves it into allow or deny.
func askDecision() tools.Decision { return tools.Decision{Action: tools.ActionAsk} }
