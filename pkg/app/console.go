package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/mcp"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/subagent"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// uiConsole implements command.Console over the objects the driver already
// holds: the UI, model registry, agent state, tool registry and session recorder.
// q is the state gate: while a drain goroutine owns State, writes queue and the
// pump applies them once it exits.
type uiConsole struct {
	ui       *hubUI
	dialogs  *interactionBroker // the brokered dialog surface, nil in tests without one
	set      *config.Set
	reg      *llm.Registry
	st       *agent.State
	tools    *tools.Registry
	commands *command.Registry
	rec      *session.Recorder
	sess     *sessRec        // the open transcript, nil when recording is off
	comp     *compactor      // nil when session recording is off
	mcp      mcpAdapter      // the MCP server manager adapter, or nil
	agents   agentsAdapter   // the sub-agent manager adapter, or nil
	permit   *permit.Barrier // live permission gate, nil when tools are disabled
	hub      *sessionHub     // control plane, set by the driver for rename tracking

	// q gates state writes against a live drain goroutine; nil applies at once
	q *steerQueue
	// snap is the settled transcript copy mid-turn readers use; nil reports none
	snap *turnSnapshot
	// deferred holds state writes queued while a turn owned State. Pump-goroutine
	// confined: command handlers append, the dispatch and drain-exit flush drain
	deferred []func()

	starts *startState // shared with the driver pump
	quit   func()      // idempotent driver quit, /exit and friends

	// refreshBase re-measures the constant request overhead and republishes the
	// bar, for the changes that resize the tool block between turns.
	refreshBase func()

	toneOnce sync.Once // the background query costs a round trip and cannot change
	tone     tui.Tone
}

func (c *uiConsole) Notify(msg string, level tui.Level) { c.ui.Notify(msg, level) }
func (c *uiConsole) Print(markdown string)              { c.ui.Print(markdown) }

func (c *uiConsole) Pick(ctx context.Context, prompt string, items []tui.PickItem, opts tui.PickOptions) (int, error) {
	return c.dialogs.Pick(ctx, prompt, items, opts)
}

func (c *uiConsole) MultiPick(ctx context.Context, prompt string, items []tui.PickItem, opts tui.MultiPickOptions) ([]int, error) {
	return c.dialogs.MultiPick(ctx, prompt, items, opts)
}

func (c *uiConsole) Select(ctx context.Context, prompt string, options []tui.Option) (int, error) {
	return c.dialogs.Select(ctx, prompt, options)
}

func (c *uiConsole) Confirm(ctx context.Context, prompt string) (bool, error) {
	return c.dialogs.Confirm(ctx, prompt)
}

func (c *uiConsole) Input(ctx context.Context, label, placeholder string) (string, error) {
	return c.dialogs.Input(ctx, label, placeholder)
}

func (c *uiConsole) InputSecret(ctx context.Context, label, placeholder string) (string, error) {
	return c.dialogs.InputSecret(ctx, label, placeholder)
}

func (c *uiConsole) ColorProfile() tui.ColorProfile { return c.ui.ColorProfile() }
func (c *uiConsole) SetTheme(pal tui.Palette)       { c.ui.SetTheme(pal) }

func (c *uiConsole) DetectTone() tui.Tone {
	c.toneOnce.Do(func() { c.tone = c.ui.DetectTone() })
	return c.tone
}

func (c *uiConsole) Models() *llm.Registry       { return c.reg }
func (c *uiConsole) State() *agent.State         { return c.st }
func (c *uiConsole) Tools() *tools.Registry      { return c.tools }
func (c *uiConsole) MCP() command.MCPServers     { return c.mcp }
func (c *uiConsole) Commands() *command.Registry { return c.commands }
func (c *uiConsole) Settings() *config.Set       { return c.set }

// SettledMessages returns the transcript for a reader that may run mid-turn:
// the live list at rest, else the copy the last turn end settled.
func (c *uiConsole) SettledMessages() []llm.Message {
	if c.st == nil {
		return nil
	}
	if !c.stateOwned() {
		return c.st.Messages // at rest, the pump serializes every reader and writer
	}
	if c.snap == nil {
		return nil // no snapshot wired, report nothing rather than a torn read
	}
	return c.snap.messages()
}

// stateOwned reports whether a drain goroutine owns or is settling State, so
// console state writes must queue behind it
func (c *uiConsole) stateOwned() bool {
	return c.q != nil && c.q.drainLive()
}

// queueState runs fn now when State is at rest, else queues it for the pump to
// land once the drain exits, and says so. Nothing of the change lands until it
// applies
func (c *uiConsole) queueState(label string, fn func()) {
	if !c.stateOwned() {
		fn()
		return
	}
	c.deferred = append(c.deferred, fn)
	if c.ui != nil {
		c.ui.Notify(label+" queued; applies after this turn", tui.LevelInfo)
	}
}

// applyDeferred lands queued state writes; a live drain keeps them queued. The
// pump runs this at each dispatch and at drain exit
func (c *uiConsole) applyDeferred() {
	if len(c.deferred) == 0 || c.stateOwned() {
		return
	}
	pending := c.deferred
	c.deferred = nil
	for _, fn := range pending {
		fn()
	}
}

// Agents returns nil when no sub-agent manager was built, so /agents reports
// unavailable instead of calling through a nil *subagent.Manager.
func (c *uiConsole) Agents() command.Agents {
	if c.agents.m == nil {
		return nil
	}
	return c.agents
}

// SaveSetting delegates to the config Set, surfacing warnings and failures.
func (c *uiConsole) SaveSetting(layer, key string, value any) error {
	warns, err := c.set.Save(layer, key, value)
	for _, w := range warns {
		c.ui.Notify(w, tui.LevelWarn)
	}
	if err != nil {
		return err
	}
	_ = layer // Save already updated the in-memory Set for future resolution
	return nil
}

func (c *uiConsole) SessionName() string {
	entries, ok := sessionEntries(c.sess)
	if !ok {
		return ""
	}
	return session.NameOf(entries)
}

func (c *uiConsole) SetSessionName(name string) error {
	if c.sess == nil || c.rec == nil {
		return errors.New("session recording is off; this session cannot be named")
	}
	canonical, err := session.ValidateName(name)
	if err != nil {
		return err
	}
	if err := c.sess.store.NameConflict(config.Cwd(), canonical, c.sess.w.Path()); err != nil {
		return err
	}
	if err := c.rec.Rename(canonical); err != nil {
		return err
	}
	// a --session create or another rename may have claimed the name between
	// the conflict check and the entry landing. Settle it like a create race.
	effective, note := renameSettle(c.sess.store, config.Cwd(), canonical, c.sess.w.Path(), c.rec)
	if note != "" {
		c.ui.Notify(note, tui.LevelWarn)
	}
	canonical = effective
	if c.hub != nil {
		c.hub.setSessionName(canonical) // snapshots report the live name
	}
	return nil
}

// keyPermissionMode is the dotted config key driving the live permission barrier.
const keyPermissionMode = "permissions.mode"

// SetSessionSetting applies a dotted key as a session override and records it so a
// resume restores it, mirroring ToolsChanged. permissions.mode is the exception: it
// drives the live barrier but never records a setting_change entry, so a resumed
// session always starts from the configured (or default) mode.
func (c *uiConsole) SetSessionSetting(key string, value any) error {
	if key == keyPermissionMode && c.permit != nil {
		s := fmt.Sprint(value)
		if m, ok := permit.ParseMode(s); ok {
			c.permit.SetMode(m)
			showPermissionIndicator(c.ui, c.permit)
		}
	}
	if key == "compaction.threshold" {
		c.queueState("compaction threshold", func() { c.applyCompactThreshold(value) })
	}
	if err := c.set.SetSession(key, value); err != nil {
		return err
	}
	if key == "images.block" {
		applyImagesBlock(c.set) // live flip, from the just-applied value
	}
	// permission mode is applied for this run only and never written to a
	// setting_change entry, so it cannot be restored on resume.
	if c.rec != nil && key != keyPermissionMode {
		return c.rec.SettingChange(key, value)
	}
	return nil
}

// ResetSessionSetting clears key's session override so lower config layers apply
// again. The reset records a null setting_change (SeedSession reads it back as a
// removal) except for permissions.mode, which never persists like its override.
func (c *uiConsole) ResetSessionSetting(key string) error {
	if err := c.set.ClearSession(key); err != nil {
		return err
	}
	if c.rec != nil && key != keyPermissionMode {
		if err := c.rec.SettingChange(key, nil); err != nil {
			return err
		}
	}
	c.reapplyResolved(key)
	return nil
}

// reapplyResolved refreshes the live surface key's override had driven, reading
// the resolved settings that now fold every lower layer. Keys with no live
// effect (limits, subagent concurrency) are read per use and need nothing.
func (c *uiConsole) reapplyResolved(key string) {
	switch {
	case key == keyPermissionMode:
		if c.permit == nil {
			return
		}
		// the resolved mode is authoritative on every start, so it is what a reset returns to
		if m, ok := permit.ParseMode(c.set.Settings().Permissions.Mode); ok {
			c.permit.SetMode(m)
			showPermissionIndicator(c.ui, c.permit)
		}
	case key == "compaction.threshold":
		v := c.set.Settings().Compaction.Threshold
		c.queueState("compaction threshold", func() { c.applyCompactThreshold(v) })
	case key == "images.block":
		applyImagesBlock(c.set)
	case key == "tools.enabled" && c.tools != nil:
		// mirrors ToolsChanged's live side effects, minus its re-recording
		c.tools.SetEnabled(c.set.Settings().Tools.Enabled)
		if c.mcp.m != nil {
			c.mcp.RefreshStatus()
		}
		if c.starts != nil && c.starts.committed.marked() && c.refreshBase != nil {
			c.refreshBase()
		}
	case key == "model":
		c.reapplyModel()
	case key == "reasoning" || strings.HasPrefix(key, "reasoning."):
		c.reapplyReasoning()
	}
}

// reapplyModel switches back onto the configured model when one resolves, so a
// reset undoes a /model override live instead of at the next start. An unset or
// unresolvable default keeps the running model.
func (c *uiConsole) reapplyModel() {
	key := c.set.Settings().Model
	if key == "" || key == c.st.Model.Key() {
		return
	}
	m, err := c.reg.Resolve(key)
	if err != nil {
		c.ui.Notify("model: configured default unavailable: "+err.Error(), tui.LevelWarn)
		return
	}
	c.queueState("model "+key, func() {
		c.applyModel(m)
		_ = c.set.ClearSession("model") // undo the override applyModel just re-pinned
	})
}

// reapplyReasoning rebuilds the live reasoning config from the resolved settings,
// mirroring what a fresh start derives.
func (c *uiConsole) reapplyReasoning() {
	rc := llm.ReasoningFrom(c.set.Settings().Reasoning, c.st.Model)
	c.queueState("reasoning", func() {
		c.st.Reasoning = rc
		c.ui.SetStatusSegment(segment(segReasoning, levelOrEmpty(rc), ""))
	})
}

// applyCompactThreshold re-defaults the registry and refreshes the live model, so a /settings edit
// moves the trigger, the context bar and the compaction tail together instead of waiting for a restart.
func (c *uiConsole) applyCompactThreshold(value any) {
	f, ok := value.(float64)
	if !ok {
		return
	}
	c.reg.SetCompactDefault(f)
	m, err := c.reg.Resolve(c.st.Model.Key())
	if err != nil {
		return
	}
	c.st.Model = m // compactor.run reads the threshold from here
	t := c.st.Tokens
	t.SetWindow(m) // keeps every context term, SetModel would blank the bar
	c.ui.SetContext(tuisink.ContextInfo(t.Context()))
}

func (c *uiConsole) SetModel(m llm.Model) {
	c.queueState("model "+m.Key(), func() { c.applyModel(m) })
}

// applyModel switches the live model, rebases the ledger and records the change.
// Picking the already-active model is a no-op: nothing to rebase, record or
// announce, so a stray /model does not spam the history with an unchanged line.
func (c *uiConsole) applyModel(m llm.Model) {
	if m.Key() == c.st.Model.Key() {
		return
	}
	// keep the stored reasoning override untouched so a temporary switch does not lose
	// the user's choice. Recompute only the live effective level for display (buildRequest
	// already clamps requests) from the raw preference, restoring it on switching back.
	lvl := llm.LevelMedium
	if parsed, ok := llm.ParseLevel(c.set.Settings().Reasoning.Level); ok {
		lvl = parsed
	}
	c.st.Model = m
	c.st.Reasoning.Level = llm.ClampLevel(m, lvl)
	c.reg.SetActive(m)
	if c.comp != nil {
		c.comp.resumeAuto() // a different window may reduce where the last one could not
	}
	showReasoningIndicator(c.ui, c.set, c.st)

	// rebase the ledger's window and reserve onto the new model so a mid-session
	// /model rescales the bar immediately rather than on the next turn.
	t := c.st.Tokens
	t.SetModel(m) // drops every context term for the new window/reserve
	c.ui.SetModelOf(m)
	// SetModel zeroed the ledger, so Used would read empty regardless of real
	// occupancy. Remeasure against the actual in-memory messages: a switch to a
	// smaller window must reflect that it now overflows, or threshold auto-compaction
	// could never fire on this model.
	t.Reseed(tokens.EstimateFor(m, c.st.Reasoning.Retain, c.st.Messages))
	c.ui.SetContext(tuisink.ContextInfo(t.Context()))
	// record a session override so Explain and Settings report (session). The
	// user-layer persist is the direct /model command's job, not this apply.
	_ = c.set.SetSession("model", m.Key())
	c.ui.Notify("model: "+m.Key(), tui.LevelInfo)

	if c.rec != nil {
		c.rec.ModelChange(m, "command")
	}
}

func (c *uiConsole) SetReasoning(rc llm.ReasoningConfig) {
	c.queueState("reasoning "+rc.Level.String(), func() { c.applyReasoning(rc) })
}

// applyReasoning sets the live reasoning config. Persistence belongs to the
// editor that owns the change, so resolved values never latch into a layer.
func (c *uiConsole) applyReasoning(rc llm.ReasoningConfig) {
	c.st.Reasoning = rc
	// keep the status indicator in step with a non-default level.
	c.ui.SetStatusSegment(segment(segReasoning, levelOrEmpty(rc), ""))
	c.ui.Notify("reasoning: "+rc.Level.String(), tui.LevelInfo)
}

func (c *uiConsole) ToolsChanged() {
	if c.tools == nil {
		return
	}
	// the dotted config key, applySetting still accepts the legacy "tools" alias
	names := c.tools.Names()
	_ = c.set.SetSession("tools.enabled", names)
	if c.rec != nil {
		_ = c.rec.SettingChange("tools.enabled", names)
	}
	if c.mcp.m != nil {
		// late registrations consult this set, so a reshape stays authoritative over
		// the snapshot taken when the run resumed
		c.mcp.SetRestore(names)
		c.mcp.RefreshStatus()
	}
	// a wider tool block occupies context immediately, not at the next turn. Before
	// the block is committed schemas are not in the base at all, so nothing to redo.
	if c.starts != nil && c.starts.committed.marked() {
		c.refreshBase()
	}
}

// Dispatched reports whether this run has sent a prompt. A resumed session
// reports false until its first prompt, keeping /tools selection free.
func (c *uiConsole) Dispatched() bool {
	return c.starts != nil && c.starts.dispatched.marked()
}

// Compact reduces the session context toward the compaction threshold. An empty
// instructions string runs an unguided pass, refused while a turn streams.
func (c *uiConsole) Compact(ctx context.Context, instructions string) error {
	if c.comp == nil {
		c.ui.Notify("compaction needs an open session", tui.LevelWarn)
		return nil
	}
	_, err := c.comp.run(ctx, agent.CompactManual, instructions)
	return err
}

func (c *uiConsole) Exit() {
	if c.quit != nil {
		c.quit()
	}
}

// turnSnapshot keeps a stable copy of the transcript as of the last turn end, the
// one source off-loop readers (/copy) may read while a turn still appends.
type turnSnapshot struct {
	agent.NopSink
	st  *agent.State
	mu  sync.Mutex
	msg []llm.Message
}

// seed installs the starting transcript, before any turn runs.
func (s *turnSnapshot) seed(msg []llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msg = msg
}

// TurnEnd snapshots the transcript on the loop goroutine, which owns it here.
func (s *turnSnapshot) TurnEnd(agent.TurnResult) {
	msg := slices.Clone(s.st.Messages)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msg = msg
}

// messages returns the last settled transcript copy.
func (s *turnSnapshot) messages() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msg
}

// mcpAdapter adapts *mcp.Manager to command.MCPServers so /mcp works without
// pkg/command importing pkg/mcp.
type mcpAdapter struct{ m *mcp.Manager }

func (a mcpAdapter) ServerNames() []string { return a.m.ServerNames() }
func (a mcpAdapter) LoadOnFirstMessage(ctx context.Context) {
	a.m.LoadOnFirstMessage(ctx)
}
func (a mcpAdapter) Status(ctx context.Context) []command.MCPServerStatus {
	src := a.m.Status(ctx)
	out := make([]command.MCPServerStatus, len(src))
	for i, s := range src {
		out[i] = command.MCPServerStatus{
			Name:      s.Name,
			Transport: s.Transport,
			State:     s.State,
			ToolCount: s.ToolCount,
			Latency:   s.Latency,
		}
	}
	return out
}
func (a mcpAdapter) Connect(ctx context.Context, name string) error { return a.m.Connect(ctx, name) }
func (a mcpAdapter) Disconnect(name string)                         { a.m.Disconnect(name) }
func (a mcpAdapter) Reload(ctx context.Context) error               { return a.m.Reload(ctx) }
func (a mcpAdapter) Logs(name string) []string                      { return a.m.Logs(name) }
func (a mcpAdapter) RefreshStatus()                                 { a.m.RefreshStatus() }

// SetRestore keeps a /tools reshape authoritative over tools that register later.
func (a mcpAdapter) SetRestore(names []string) { a.m.SetRestore(names) }

// Groups maps the manager's tool groups onto /tools headers.
func (a mcpAdapter) Groups() []command.MCPGroup {
	src := a.m.ToolGroups()
	out := make([]command.MCPGroup, len(src))
	for i, g := range src {
		out[i] = command.MCPGroup{Source: g.Source, Label: g.Label}
	}
	return out
}

// agentsAdapter adapts *subagent.Manager to command.Agents so /agents works
// without pkg/command importing pkg/subagent.
type agentsAdapter struct{ m *subagent.Manager }

func (a agentsAdapter) List() []command.AgentJob {
	src := a.m.List()
	out := make([]command.AgentJob, len(src))
	for i, j := range src {
		out[i] = command.AgentJob{
			ID:      j.ID,
			Status:  j.Status.String(),
			Task:    j.Task,
			Elapsed: j.Elapsed(), // queue wait for queued/aborted-before-run, else active runtime
		}
	}
	return out
}
func (a agentsAdapter) Stop(id string) error { return a.m.Stop(id) }
func (a agentsAdapter) StopAll() int         { return a.m.StopAll() }

// registryAdapter adapts *tools.Registry to mcp.Registrar so pkg/mcp never imports
// pkg/tools. The State enums are numerically identical, so a direct cast bridges them.
type registryAdapter struct{ reg *tools.Registry }

func (a registryAdapter) RegisterState(source string, t agent.Tool, s mcp.State) {
	a.reg.RegisterState(source, t, tools.State(s))
}
func (a registryAdapter) Unregister(source string)            { a.reg.Unregister(source) }
func (a registryAdapter) EnabledNames(source string) []string { return a.reg.EnabledNames(source) }
func (a registryAdapter) DisabledNames(source string) []string {
	return a.reg.DisabledNames(source)
}
func (a registryAdapter) AllNames(source string) []string { return a.reg.AllNames(source) }
func (a registryAdapter) MarkReadOnly(names []string)     { a.reg.MarkReadOnly(names) }

// levelOf maps a warn flag onto the matching notice level.
func levelOf(warn bool) tui.Level {
	if warn {
		return tui.LevelWarn
	}
	return tui.LevelInfo
}

// levelOrEmpty returns the reasoning segment text, or "" at the default level.
func levelOrEmpty(rc llm.ReasoningConfig) string {
	if rc.Level == llm.LevelMedium {
		return ""
	}
	return rc.Level.String()
}

// showReasoningIndicator shows the reasoning level in the status bar when it
// differs from the resolved default, clearing it otherwise.
func showReasoningIndicator(ui *hubUI, set *config.Set, st *agent.State) {
	var text string
	if d, _, ok := set.Explain("reasoning.level"); ok && string(d) != `"medium"` {
		text = st.Reasoning.Level.String() // an explicit config choice stays visible even when clamped
	} else {
		text = levelOrEmpty(st.Reasoning)
	}
	ui.SetStatusSegment(segment(segReasoning, text, ""))
}
