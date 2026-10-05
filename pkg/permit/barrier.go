package permit

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tools"
)

// unattendedDenyReason is the one refusal for an offered call nobody approved:
// a denied or failed review, an unparseable shell line and a prompter that
// cannot open all read the same, so a script never parses why.
const unattendedDenyReason = "permission not given"

// maxClassifierArgs bounds a non-bash call's payload sent to auto classification,
// so a large write body never inflates the model request.
const maxClassifierArgs = 500

// Barrier gates every tool call through static classification plus an optional
// approval dialog, holding the live mode and any open dialogs.
type Barrier struct {
	mu         sync.Mutex
	mode       Mode
	prompter   Prompter
	noter      Noter
	classifier Classifier
	notice     func(string)               // transient UI notices (auto-allow), nil = none
	dryRun     func(agent.ToolCall) error // registry-backed, nil means cannot predict
	ro         func(string) bool
	safe       func(agent.ToolCall) bool // config-declared safe commands, nil = none
	deny       func(agent.ToolCall) bool // config-declared denied commands, nil = none
	scope      writeScope                // auto+write's writable roots, zero allows nothing

	preview func(agent.ToolCall) string // enhanced dialog subject, nil = raw arguments

	grants []string        // construction grants, re-applied after every mode change
	allows map[string]bool // session allows by allowSessionKey
	open   []*pendingAsk   // live dialogs, re-evaluated on mode change
	warm   map[string]context.CancelFunc
}

// pendingAsk tracks one open approval dialog so a mode change can resolve it.
type pendingAsk struct {
	call       agent.ToolCall
	dlg        Dialog
	auto       bool   // an allow verdict resolved this dialog (auto mode)
	cancelAuto func() // stops a racing classification, nil when none runs
}

// Options carries the decision inputs a front end installs at construction.
// Mode names the starting gate, ModeSet false leaving the default allow-read.
// Grants pre-populate the session-allow memory and survive the mode changes
// that clear earned grants.
type Options struct {
	Mode           Mode // starting gate, ignored unless ModeSet
	ModeSet        bool // whether Mode overrides the default
	Grants         []string
	Prompter       Prompter     // approval dialogs, nil meaning unattended
	Noter          Noter        // allow/deny note injection, nil drops notes
	Classifier     Classifier   // auto-mode verdicts, nil meaning none
	Notice         func(string) // transient status notices, nil silences them
	DryRun         func(agent.ToolCall) error
	Preview        func(agent.ToolCall) string
	SafeCommands   []string // exact tool names or bash lines skipping the prompt
	DeniedCommands []string // exact tool names or bash lines refused in every mode
	WriteRoots     []string // auto+write's writable roots, empty allowing none
}

// NewBarrier builds a barrier with read-only metadata lookup ro, applying o's
// inputs with grants last so the starting mode can never clear them.
func NewBarrier(ro func(string) bool, o Options) *Barrier {
	b := &Barrier{
		mode:       ModeAllowRead,
		allows:     make(map[string]bool),
		warm:       make(map[string]context.CancelFunc),
		ro:         ro,
		grants:     o.Grants,
		prompter:   o.Prompter,
		noter:      o.Noter,
		classifier: o.Classifier,
		notice:     o.Notice,
		dryRun:     o.DryRun,
		preview:    o.Preview,
	}
	if len(o.SafeCommands) > 0 {
		b.safe = func(call agent.ToolCall) bool { return SafeMatches(call, o.SafeCommands) }
	}
	if len(o.DeniedCommands) > 0 {
		b.deny = func(call agent.ToolCall) bool { return DenyMatches(call, o.DeniedCommands) }
	}
	if len(o.WriteRoots) > 0 {
		b.scope = newWriteScope(o.WriteRoots[0], o.WriteRoots[1:]...)
	}
	if o.ModeSet {
		b.mode = o.Mode
	}
	b.grantSessionAllowsLocked(o.Grants)
	return b
}

// grantSessionAllowsLocked keys each name as both a tool grant and a bash
// head grant, so a tool name that is also a command head covers both shapes.
// Caller holds the lock.
func (b *Barrier) grantSessionAllowsLocked(names []string) {
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		b.allows[n] = true
		if n != tools.ToolBash {
			b.allows["bash:"+n] = true
		}
	}
}

// Mode returns the current live mode.
func (b *Barrier) Mode() Mode {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.mode
}

// SetMode swaps the live mode and re-evaluates any open dialog under it: a call
// the new mode would allow outright is resolved as allow, while a mode change
// away from the deciding auto mode cancels its racing classification so the
// user decides. Never rewrites config.
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
	b.reevaluateOpens(m, opens)
}

// Cycle advances to the next mode in order and re-evaluates open dialogs.
func (b *Barrier) Cycle() Mode { return b.rotate(b.mode.Next()) }

// Prev steps back one mode in cycle order, with Cycle's dialog re-evaluation
// and session-allow reset.
func (b *Barrier) Prev() Mode { return b.rotate(b.mode.Prev()) }

// rotate applies the neighbouring mode m: session allows never cross a mode
// change, and open dialogs re-evaluate under m.
func (b *Barrier) rotate(m Mode) Mode {
	b.mu.Lock()
	b.mode = m
	b.resetSessionAllowsLocked() // a new mode never inherits the previous gate's approvals
	opens := slices.Clone(b.open)
	b.mu.Unlock()

	b.reevaluateOpens(m, opens)
	return m
}

// reevaluateOpens settles dialogs still open after a mode change: the new mode's
// static verdict may allow one outright, and any racing auto classification is
// cancelled since the mode that launched it no longer decides.
func (b *Barrier) reevaluateOpens(m Mode, opens []*pendingAsk) {
	b.mu.Lock()
	cancels := make([]func(), 0, len(opens))
	for _, pa := range opens {
		if pa.cancelAuto != nil {
			cancels = append(cancels, pa.cancelAuto)
			pa.cancelAuto = nil
		}
	}
	b.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}

	g := b.gateFor(m)
	for _, pa := range opens {
		if g.staticVerdict(context.Background(), pa.call).Action == tools.ActionAllow {
			pa.dlg.Resolve(int(optAllow))
		}
	}
}

// resetSessionAllowsLocked clears earned session memory so a mode change never
// carries one gate level's approvals into another, then re-applies the
// construction grants, which hold across modes. Caller holds the lock.
func (b *Barrier) resetSessionAllowsLocked() {
	clear(b.allows)
	b.grantSessionAllowsLocked(b.grants)
}

// Guard returns the static gate: user-initiated and allow-all always permit,
// rejections deny with guidance, and block-all asks even for reads. Never blocks.
func (b *Barrier) Guard() tools.Guard {
	return func(ctx context.Context, call agent.ToolCall) tools.Decision {
		return b.gateNow().staticVerdict(ctx, call)
	}
}

// Prefetch starts classification for every call in a batch that will actually
// reach the model classifier, warming the verdict cache so the asker consumes a
// ready verdict behind its typing hold instead of waiting on a fresh request,
// including a lone eligible call, since serial predecessors may run for a while
// before it asks. It filters exactly as the unattended ask does: only auto-mode
// bash lines static analysis can name and non-write extension calls whose
// static verdict is Ask and which are not already session-allowed go to the
// model. Identical subjects launch one request. Launched goroutines observe ctx
// (the turn's), so an abort stops them, and answering the dialog a request
// fronts cancels it. Never blocks.
func (b *Barrier) Prefetch(ctx context.Context, calls []agent.ToolCall) {
	if b.classifier == nil {
		return
	}
	g := b.gateNow()
	for _, call := range calls {
		if !b.classifyCall(g.mode, call.Name) {
			continue // core writer or non-auto mode: never classified at ask time either
		}
		if call.Name == tools.ToolBash {
			if _, ok := sessionNames(bashCommand(call.Input)); !ok {
				continue // unparseable line: the unattended ask refuses it, never the model
			}
		}
		if g.staticVerdict(ctx, call).Action != tools.ActionAsk {
			continue // statically resolved (read-only, config safe/deny, write scope): no model
		}
		if _, ok := b.sessionAllowed(call); ok {
			continue // an allow-for-session grant already covers it: no dialog, no model
		}
		b.startWarm(ctx, classifySubject(g.mode, call))
	}
}

// startWarm launches one prefetched classification for s, deduped by subject.
func (b *Barrier) startWarm(ctx context.Context, s Subject) {
	key := s.key()
	wctx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	if _, ok := b.warm[key]; ok {
		b.mu.Unlock()
		cancel() // one request serves every dialog with this subject
		return
	}
	b.warm[key] = cancel
	b.mu.Unlock()
	go func() {
		defer cancel()
		b.classifier.Classify(wctx, s) // warms the LRU, unsure never cached
		b.mu.Lock()
		delete(b.warm, key) // only the owner removes, startWarm never overwrites
		b.mu.Unlock()
	}()
}

// cancelWarm stops the prefetched classification for s, if one still runs.
func (b *Barrier) cancelWarm(s Subject) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if cancel, ok := b.warm[s.key()]; ok {
		cancel()
	}
}

// cachedVerdict returns a verdict already banked for s, ok false for a bare fn
// classifier or a miss. It never blocks and never starts a request.
func (b *Barrier) cachedVerdict(s Subject) (Class, bool) {
	c, ok := b.classifier.(*cachedClassifier)
	if !ok {
		return ClassUnsure, false
	}
	return c.peek(s)
}

// holdForDialog waits out typing so the next Open never steals focus from a
// draft, returning early on an allow verdict. It returns the unconsumed verdict
// channel (nil once a verdict landed) and whether that verdict allows.
// verdict may be nil, which never selects.
func holdForDialog(ctx context.Context, p Prompter, verdict <-chan Class) (<-chan Class, bool) {
	holdCtx, cancel := context.WithCancel(ctx)
	defer cancel() // releases the hold when a verdict wins the race
	held := make(chan struct{})
	go func() { p.Hold(holdCtx); close(held) }()
	for {
		select {
		case <-ctx.Done(): // a stuck Hold must not deadlock the ask
			return nil, false
		case <-held: // typing settled, the dialog may open
			select { // a verdict racing the pause settles here, not in a flash dialog
			case c := <-verdict:
				return nil, c == ClassAllow && ctx.Err() == nil
			default:
				return verdict, false
			}
		case c := <-verdict:
			if c == ClassAllow && ctx.Err() == nil {
				return nil, true
			}
			verdict = nil // deny/unsure needs a person, keep holding
		}
	}
}

// Asker resolves a guard's ask into allow or deny. It consults session allows,
// holds the prompter while the user types, then opens an approval dialog. In the
// auto modes a concurrent classification can approve the call without a dialog.
func (b *Barrier) Asker() tools.Asker {
	return func(ctx context.Context, call agent.ToolCall, _ tools.Decision) tools.Decision {
		g := b.gateNow()
		m := g.mode

		if _, ok := b.sessionAllowed(call); ok {
			return allowDecision()
		}

		prompter, _ := b.prompterSnapshot()
		if prompter == nil { // nobody to ask; the auto modes still get a verdict
			return b.unattendedAsk(ctx, m, call)
		}

		// the auto modes classify the call while the user types, the verdict racing
		// the hold and the open dialog. A verdict Prefetch already banked answers
		// synchronously (no goroutine), an allow running the tool without a draft.
		var classifierCtx context.Context
		cancel := func() {}
		var subject Subject
		var verdict <-chan Class
		if b.classifyCall(m, call.Name) {
			subject = classifySubject(m, call)
			if c, ok := b.cachedVerdict(subject); ok {
				// banked: allow approves at once, deny/unsure still needs a person
				if c == ClassAllow && ctx.Err() == nil && b.Mode() == m {
					b.resolveNotice("once", true)
					return allowDecision()
				}
			} else {
				classifierCtx, cancel = context.WithCancel(ctx)
				ch := make(chan Class, 1)
				verdict = ch
				go func() { ch <- b.classifier.Classify(classifierCtx, subject) }()
			}
		}
		stop := func() { cancel(); b.cancelWarm(subject) }

		// the hold defers the dialog while the user types; an allow verdict ends it
		// at once, so an AI-approved tool runs mid-keystroke
		var allowed bool
		verdict, allowed = holdForDialog(ctx, prompter, verdict)
		if (verdict != nil || allowed) && b.Mode() != m {
			// a mode change since the snapshot: the racing (or winning) verdict no
			// longer speaks for the gate, so only a person or the new mode may allow
			stop()
			verdict, allowed = nil, false
		}
		if allowed {
			stop()
			b.resolveNotice("once", true)
			return allowDecision()
		}
		if ctx.Err() != nil { // aborted during the hold, nobody left to ask
			stop()
			return tools.Deny(unattendedDenyReason)
		}

		dlg, err := prompter.Open(promptText(m, call.Name), b.dialogSubject(call), buildOptions(bashCommand(call.Input)))
		if err != nil || dlg == nil {
			// no dialog can open (plain render, a UI that went away): settle the ask
			// exactly as a run without a prompter would, so permission behavior follows
			// "can this session open a dialog", never which front end is attached
			d := b.unattendedAsk(ctx, m, call)
			stop()
			return d
		}

		b.mu.Lock()
		pa := &pendingAsk{call: call, dlg: dlg}
		if verdict != nil && classifierCtx.Err() == nil {
			pa.cancelAuto = cancel // registered with the dialog so SetMode cannot miss it
		}
		b.open = append(b.open, pa)
		mNow := b.mode                             // same-lock capture so a concurrent SetMode/Cycle is ordered against registration
		stale := mNow != m && pa.cancelAuto != nil // SetMode swept before this dialog existed
		if stale {
			pa.cancelAuto = nil
		}
		b.mu.Unlock()

		// stop() at Wait's return cancels again; context cancel is idempotent
		if stale {
			cancel() // the mode that launched the verdict no longer decides
		}

		// a Shift+Tab landed between Open and registration, so re-evaluate under the new mode.
		if mNow != m && b.gateFor(mNow).staticVerdict(ctx, call).Action == tools.ActionAllow {
			dlg.Resolve(int(optAllow))
		}

		if verdict != nil && classifierCtx.Err() == nil {
			go func() {
				// a user answer cancels the context, skipping both the resolve and its
				// auto-allowed report so a denial is never claimed as auto-allowed.
				if c := <-verdict; c != ClassAllow || classifierCtx.Err() != nil {
					return
				}
				b.mu.Lock()
				pa.auto = true // set before Resolve so Wait's return already sees it
				b.mu.Unlock()
				dlg.Resolve(int(optAllow)) // first resolver wins, a keystroke beats this
			}()
		}

		idx, werr := dlg.Wait(ctx)
		stop() // the user answered or gave up, stop the classification and its prefetch

		b.mu.Lock()
		auto := pa.auto
		b.open = bulk.SliceFilterInPlace(func(p *pendingAsk) bool { return p != pa }, b.open)
		b.mu.Unlock()

		if werr != nil {
			if errors.Is(werr, ErrDenied) {
				return tools.Deny("denied by user")
			}
			return tools.Deny(unattendedDenyReason)
		}
		return b.resolveChoice(ctx, call, idx, auto)
	}
}

// unattendedAsk decides a prompted call with no UI: the auto modes take the
// model verdict as final, every other mode has nobody to decide.
func (b *Barrier) unattendedAsk(ctx context.Context, m Mode, call agent.ToolCall) tools.Decision {
	// a core writer never classifies, so an offered one outside auto+write's scope
	// is withheld by the gate itself rather than left to a missing UI
	if _, isWrite := coreWriteTools[call.Name]; isWrite && (m == ModeAuto || m == ModeAutoWrite) {
		return tools.Deny(unattendedDenyReason)
	}
	if !b.classifyCall(m, call.Name) {
		return tools.Deny(unattendedDenyReason)
	}
	// a line static analysis cannot parse (redirect, substitution, unnameable
	// head) never matches a grant either, so unattended it refuses outright
	// rather than leaning on a model reading raw text
	if call.Name == tools.ToolBash {
		if _, ok := sessionNames(bashCommand(call.Input)); !ok {
			return tools.Deny(unattendedDenyReason)
		}
	}
	switch b.classifier.Classify(ctx, classifySubject(m, call)) {
	case ClassAllow:
		if ctx.Err() != nil { // the run ended under the verdict, nothing left to allow
			return tools.Deny(unattendedDenyReason)
		}
		b.resolveNotice("once", true)
		return allowDecision()
	default:
		// a deny verdict and an errored review both leave no permission: fail closed
		return tools.Deny(unattendedDenyReason)
	}
}

// resolveChoice maps a dialog answer to an allow/deny decision and applies its
// side effects (session memory, note injection). displayIdx is the position in
// the rendered option list, translated through optionActions so Deny on a plain
// command (whose list drops the session option) still refuses.
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
// or one "bash:<name>" per identifiable non-readonly command. (nil,false) when the
// line offers no session memory (redirect/substitution, unnameable head).
func (b *Barrier) allowSessionKeys(call agent.ToolCall) ([]string, bool) {
	if call.Name != tools.ToolBash {
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
// is empty when no grant matched, ok distinguishing a match from none. A named grant
// covers a plain command and any compound whose non-readonly heads are all granted;
// the bare `bash` grant covers every nameable shell call; a line no grant covers
// (redirect/substitution, unnameable head) never matches, so it re-prompts every time.
func (b *Barrier) sessionAllowed(call agent.ToolCall) (string, bool) {
	cmd := bashCommand(call.Input)
	if call.Name != tools.ToolBash || !compound(cmd) { // plain command or non-bash tool
		key := allowSessionKey(call)

		b.mu.Lock()
		defer b.mu.Unlock()

		if b.allows[key] {
			return key, true
		}
		return key, call.Name == tools.ToolBash && b.allows[tools.ToolBash]
	}

	heads, ok := compoundGoverningHeads(cmd)
	if !ok || len(heads) == 0 {
		return "", false // unidentifiable: no grant can cover it, re-prompt
	}

	b.mu.Lock()
	granted := maps.Clone(b.allows)
	b.mu.Unlock()

	for _, h := range heads {
		// the bare `bash` grant stands in for any head a compound could name
		if !granted["bash:"+h] && !granted[tools.ToolBash] {
			return "", false // an ungranted head: re-prompt
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
// call was refused, mirroring allow-with-note, an empty reason injecting nothing.
func (b *Barrier) noteDenied(call agent.ToolCall, reason string) {
	if b.noter == nil || strings.TrimSpace(reason) == "" {
		return
	}
	b.noter("Denied with note: " + strings.TrimSpace(reason))
}

// classifyCall reports whether mode sends this call to the model classifier:
// the auto modes judge bash and MCP/extension calls (with their metadata). A
// core writer never goes to the model, and a nil classifier starts none.
func (b *Barrier) classifyCall(m Mode, name string) bool {
	if b.classifier == nil {
		return false
	}
	switch m {
	case ModeAuto, ModeAutoWrite:
		if name == tools.ToolBash {
			return true
		}
		_, isWrite := coreWriteTools[name]
		return !isWrite // MCP and other extension tools, judged with their metadata
	default:
		return false
	}
}

// classifySubject builds the classifier's subject for call: the bash command,
// or the tool name plus its elided arguments for any other (MCP) tool.
// AllowWrite selects the workspace rule set, and only for a shell command.
func classifySubject(m Mode, call agent.ToolCall) Subject {
	if call.Name == tools.ToolBash {
		// the declared cwd rebases every relative path, so the model must see it
		return Subject{
			Name:       tools.ToolBash,
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
		// a config-declared safe command skips the prompt, otherwise ask. A hard
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
// Unlike SafeMatches it may also name core writers, denying one being a legitimate safety gate.
func DenyMatches(call agent.ToolCall, cmds []string) bool {
	for _, e := range cmds {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if call.Name != tools.ToolBash && toolNameCovered(call.Name, e) {
			return true
		}
	}
	if call.Name == tools.ToolBash {
		s := scanCommand(bashCommand(call.Input))
		for i, seg := range s.Segments {
			var raw string
			if i < len(s.Raw) {
				raw = s.Raw[i]
			}
			if entryCovered(seg, cmds) || entryCoversArgv(raw, cmds) {
				return true
			}
			if deniedPayload(raw, cmds) {
				return true // the interpreter's own tokens hide what it runs
			}
		}
	}
	return false
}

// deniedPayload reports whether a nested-interpreter segment (sh -c, eval) wraps a
// command the deny list names. The collapsed scan never sees the payload, so it is
// pulled from the verbatim text and checked directly. eval's arguments are plain
// words of this line, so they are scanned again for their own splits.
func deniedPayload(raw string, cmds []string) bool {
	for _, p := range interpreterPayloads(raw) {
		ps := scanCommand(strings.TrimSpace(p))
		for i, seg := range ps.Segments {
			if entryCovered(seg, cmds) {
				return true
			}
			if i < len(ps.Raw) && entryCoversArgv(ps.Raw[i], cmds) {
				return true
			}
		}
	}
	return false
}

// entryCoversArgv reports whether the resolved tokens of raw start with any
// configured entry's resolved tokens, so quoting or escaping the head
// ("git" push, git pus\x68) cannot slip a listed command past the list. Both
// sides resolve through tokenizeRaw, the same argv view the sed/awk/git checkers
// match on. Token-exact: an entry longer than the line cannot match.
func entryCoversArgv(raw string, cmds []string) bool {
	if raw == "" {
		return false
	}
	toks := tokenizeRaw(raw)
	if len(toks) == 0 {
		return false
	}
	toks[0] = stripPath(toks[0])
	for _, e := range cmds {
		et := tokenizeRaw(strings.TrimSpace(e))
		if len(et) == 0 || len(et) > len(toks) {
			continue
		}
		if slices.Equal(toks[:len(et)], et) {
			return true
		}
	}
	return false
}

// CommandRefused reports whether the deny configuration refuses bash cmd at
// all: the bare invocation matches an entry, or an entry runs cmd with
// arguments. Callers advertising example commands use it to hide anything the
// barrier would question.
func CommandRefused(cmd string, denied []string) bool {
	if len(denied) == 0 {
		return false
	}
	if entryCovered(cmd, denied) {
		return true
	}
	for _, e := range denied {
		if fields := strings.Fields(e); len(fields) > 0 && fields[0] == cmd {
			return true
		}
	}
	return false
}

// SafeMatches reports whether call is named by a configured safe command: an exact
// tool name (or an MCP server namespace, covering every `srv__*` tool it exposes)
// for any non-bash tool, or, for bash, the trimmed command line matched as a
// token-boundary prefix, so "git" covers every git invocation and "git status"
// its subcommands. A compound line matches only when every component is either a
// listed entry or verifiably read-only (mirroring allSegmentsReadOnly's
// all-or-nothing gate), so an appended write never rides in. write/edit can never
// be listed, so no config entry overrides a known writer.
func SafeMatches(call agent.ToolCall, cmds []string) bool {
	if _, isWrite := coreWriteTools[call.Name]; isWrite {
		return false
	}
	for _, e := range cmds {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if call.Name != tools.ToolBash && toolNameCovered(call.Name, e) {
			return true
		}
	}
	if call.Name == tools.ToolBash {
		return safeBashLine(bashCommand(call.Input), cmds)
	}
	return false
}

// safeBashLine reports whether a bash line is covered by configured entries. A single
// command matches on the token-boundary prefix of its trimmed text, while a compound (control
// operators or substitution) requires every component to be either a listed entry
// or verifiably read-only, so "make lint" can never smuggle in an appended write.
func safeBashLine(cmd string, cmds []string) bool {
	s := scanCommand(cmd)
	if s.HasUnsafeOp { // > ` $( <( defeat analysis, fail to the prompt path
		return false
	}
	if !s.HasSplitOp && len(s.Segments) <= 1 {
		return entryCovered(strings.TrimSpace(cmd), cmds) || entryCoversArgv(cmd, cmds)
	}
	for i, seg := range s.Segments {
		var rw string
		if i < len(s.Raw) { // Segments and Raw stay index-aligned from pushSegment
			rw = s.Raw[i]
		}
		if entryCovered(seg, cmds) || entryCoversArgv(rw, cmds) || segmentIsReadOnly(seg, rw) {
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
	switch rest[0] { // a boundary ends the matched token, a letter continues it
	case ' ', '\t', ';', '|', '&', '<', '>':
		return true
	default:
		return false
	}
}

// rejectionReason names what was refused and why, guiding the model.
func rejectionReason(call agent.ToolCall) string {
	if call.Name == tools.ToolBash {
		return "refused: in-place write (sed -i); use the edit tool instead"
	}
	return "refused: " + call.Name
}

// deniedReason names a config-denied command and why, guiding the model.
func deniedReason(call agent.ToolCall) string {
	if call.Name == tools.ToolBash {
		return "denied by configuration, ask user to run if necessary"
	}
	return "refused: " + call.Name
}

// promptText is the dialog's question line for a mode and tool.
func promptText(m Mode, name string) string {
	if m == ModeBlockAll { // block-all genuinely prompts everything
		return "block-all permits nothing without approval. Run this?"
	}
	return fmt.Sprintf("Allow `%s` tool call?", name)
}

// allowDecision builds an allow decision (no reason needed).
func allowDecision() tools.Decision { return tools.Decision{Action: tools.ActionAllow} }

// askDecision asks for approval, the asker resolving it into allow or deny.
func askDecision() tools.Decision { return tools.Decision{Action: tools.ActionAsk} }
