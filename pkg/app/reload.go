package app

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// projectStore owns the AGENTS.md content the agent and every sub-agent read, so
// /reload swaps it in one place.
type projectStore struct {
	mu sync.RWMutex
	p  []agent.ProjectInstruction
}

func (s *projectStore) get() []agent.ProjectInstruction {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.p
}

func (s *projectStore) set(p []agent.ProjectInstruction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p = p
}

// startupKeys are the settings copied by value into objects built before the
// console exists, so an edit to them can only be reported, never applied: UI,
// keymap and image-protocol setup, agent loop bounds, sub-agent manager fields, the
// tool registry's advertised commands and diff echo, the barrier's construction-time
// lists, and the control socket. disableUpdateCheck is absent on purpose: it gates
// only the startup probe, so an edit has nothing to restart into.
type startupKeys struct {
	render        string
	color         string
	images        string
	keySubmit     []string
	keyNewline    []string
	maxSteps      int
	turnRetries   int
	systemPrompt  string
	shellCommands []string
	diffEcho      string
	maxConcurrent int
	subMaxSteps   int
	subPollWait   string
	safe          []string
	denied        []string
	control       bool
}

// startupKeysOf snapshots the keys a reload cannot reach.
func startupKeysOf(s config.Settings) startupKeys {
	return startupKeys{
		render:        s.UI.Render,
		color:         s.UI.Color,
		images:        s.UI.Images,
		keySubmit:     s.UI.Keymap.Submit,
		keyNewline:    s.UI.Keymap.Newline,
		maxSteps:      s.Agent.MaxSteps,
		turnRetries:   s.Agent.TurnRetries,
		systemPrompt:  s.Agent.SystemPrompt,
		shellCommands: s.Tools.ShellCommands,
		diffEcho:      s.Tools.DiffEcho,
		maxConcurrent: s.Subagent.MaxConcurrent,
		subMaxSteps:   s.Subagent.MaxSteps,
		subPollWait:   s.Subagent.MaxPollWait,
		safe:          s.Permissions.SafeCommands,
		denied:        s.Permissions.DeniedCommands,
		control:       s.Control.Enabled,
	}
}

// changed names the dotted keys whose value moved between two snapshots.
func (k startupKeys) changed(now startupKeys) []string {
	var out []string
	if k.render != now.render {
		out = append(out, "ui.render")
	}
	if k.color != now.color {
		out = append(out, "ui.color")
	}
	if !slices.Equal(k.keySubmit, now.keySubmit) {
		out = append(out, "ui.keymap.submit")
	}
	if !slices.Equal(k.keyNewline, now.keyNewline) {
		out = append(out, "ui.keymap.newline")
	}
	if k.images != now.images {
		out = append(out, "ui.images")
	}
	if k.maxSteps != now.maxSteps {
		out = append(out, "agent.maxSteps")
	}
	if k.turnRetries != now.turnRetries {
		out = append(out, "agent.turnRetries")
	}
	if k.systemPrompt != now.systemPrompt {
		out = append(out, "agent.systemPrompt")
	}
	if !slices.Equal(k.shellCommands, now.shellCommands) {
		out = append(out, "tools.shellCommands")
	}
	if k.diffEcho != now.diffEcho {
		out = append(out, "tools.diffEcho")
	}
	if k.maxConcurrent != now.maxConcurrent {
		out = append(out, "subagent.maxConcurrent")
	}
	if k.subMaxSteps != now.subMaxSteps {
		out = append(out, "subagent.maxSteps")
	}
	if k.subPollWait != now.subPollWait {
		out = append(out, "subagent.maxPollWait")
	}
	if !slices.Equal(k.safe, now.safe) {
		out = append(out, "permissions.safeCommands")
	}
	if !slices.Equal(k.denied, now.denied) {
		out = append(out, "permissions.deniedCommands")
	}
	if k.control != now.control {
		out = append(out, "control.enabled")
	}
	return out
}

// reloadDeps are the live objects /reload re-applies onto that the console does
// not otherwise own. A nil func is skipped.
type reloadDeps struct {
	// running reports State owned by a drain goroutine: /reload refuses up front,
	// and one starting mid-reload finds the state writes below queued instead
	running func() bool                           // nil skips the check
	project func([]agent.ProjectInstruction) bool // system-block instructions; false while a turn runs
	startup startupKeys                           // values as of the last start or reload
}

// Reload re-reads config.json, models.json, mcp.json and AGENTS.md and re-applies
// them to the live session. It writes nothing, so session overrides still outrank
// every file layer, and it never enables a tool: newly discovered MCP tools land
// disabled for /tools to pick up.
func (c *uiConsole) Reload(ctx context.Context) error {
	if c.reload.running != nil && c.reload.running() {
		c.ui.Notify("reload needs an idle turn; try again once this one ends", tui.LevelWarn)
		return nil
	}
	warns, err := c.set.Reload()
	c.warn(warns)
	if err != nil {
		return err // the previous configuration is still live
	}
	s := c.set.Settings()

	c.reloadModels(s)   //nolint:contextcheck // discovery is self-bounded by DiscoverTimeout
	c.reloadSettings(s) //nolint:contextcheck // SetMode is synchronous and deliberately ctx-less
	c.reloadProject()
	c.reloadMCP(ctx)

	now := startupKeysOf(s)
	if keys := c.reload.startup.changed(now); len(keys) > 0 {
		c.reload.startup = now // report an edit once, not on every later reload
		c.ui.Notify("reload: "+strings.Join(keys, ", ")+" need a restart", tui.LevelWarn)
	}
	if c.refreshBase != nil && c.starts != nil && c.starts.committed.marked() {
		c.refreshBase() // the base block is committed, so a resized one must republish
	}
	c.ui.Notify("reloaded config.json, models.json, mcp.json and AGENTS.md", tui.LevelInfo)
	return nil
}

// reloadModels re-reads models.json and config overrides onto the live registry,
// then starts background discovery so a newly added provider lists its models
// without blocking the command.
func (c *uiConsole) reloadModels(s config.Settings) {
	if c.reg == nil {
		return
	}
	file, warns, err := llm.LoadUserFile()
	c.warn(warns)
	if err != nil {
		c.ui.Notify("models: "+err.Error(), tui.LevelWarn)
		return
	}
	if s.Model != "" { // config.json's model override wins over the file default
		file.DefaultModel = s.Model
	}
	c.warn(c.reg.SetFile(file))

	// unconditional: the same key can now carry a different window or threshold.
	// No config-pinned model rebases onto the registry's own default.
	key := s.Model
	if key == "" {
		if a := c.reg.Active(); a.ID != "" {
			key = a.Key()
		}
	}
	if m, rerr := c.reg.Resolve(key); rerr == nil {
		// queueState over applyState: the idle check is only a snapshot, so a drain
		// starting during these reads defers the write instead of racing State
		c.queueState("model "+key, func() { c.applyModel(m) })
	} else if s.Model != "" { // a stale active model keeps serving by design; a named one warns
		c.ui.Notify("model: configured default unavailable: "+rerr.Error(), tui.LevelWarn)
	}
	go func() {
		notify := func(msg string, warn bool) { c.ui.Notify(msg, levelOf(warn)) }
		if added := c.reg.RefreshModels(notify); added > 0 && c.ui != nil {
			c.ui.NotifyKeyed("models", "discovered "+strconv.Itoa(added)+" more models", tui.LevelInfo)
		}
	}()
}

// reloadSettings re-applies the merged settings live objects can take on the
// spot. The compaction threshold rides applyCompactThreshold so it reaches both
// the registry default and st.Model even when a stale active model keeps serving.
// tools.enabled is deliberately absent: a reload never enables a tool.
func (c *uiConsole) reloadSettings(s config.Settings) {
	tools.ApplyLimits(tools.LimitsFrom(s.Tools.Limits))
	applyImagesBlock(c.set)
	if pal, ok := tui.LookupPalette(s.UI.Theme); ok {
		c.ui.SetTheme(pal)
	}
	if c.permit != nil {
		if m, ok := permit.ParseMode(s.Permissions.Mode); ok {
			c.permit.SetMode(m)
			showPermissionIndicator(c.ui, c.permit)
		}
	}
	if c.st != nil {
		v := s.Compaction.Threshold
		c.queueState("compaction threshold", func() { c.applyCompactThreshold(v) })
		c.queueState("reasoning", func() { // pairs the write with the indicator's read of it
			c.st.Reasoning = llm.ReasoningFrom(s.Reasoning, c.st.Model)
			showReasoningIndicator(c.ui, c.set, c.st)
		})
	}
}

// reloadProject re-reads AGENTS.md into the system block, at the cost of one
// prompt-cache miss on the next request.
func (c *uiConsole) reloadProject() {
	if c.reload.project == nil {
		return
	}
	globalDir, _ := config.Home()
	proj, err := agent.LoadProjectInstructions(globalDir, config.Cwd())
	if err != nil {
		c.ui.Notify("could not read AGENTS.md: "+err.Error(), tui.LevelWarn)
		return
	}
	if !c.reload.project(proj) {
		c.ui.Notify("AGENTS.md not reloaded; a turn started", tui.LevelWarn)
	}
}

// reloadMCP reconciles mcp.json with the session's tool selection, so what the
// edit adds registers disabled and nothing already live is turned off.
func (c *uiConsole) reloadMCP(ctx context.Context) {
	if c.mcp.m == nil {
		return
	}
	if err := c.mcp.Reload(ctx); err != nil {
		c.ui.Notify("mcp: "+err.Error(), tui.LevelWarn)
	}
}

// warn surfaces every message as a warning notice.
func (c *uiConsole) warn(msgs []string) {
	for _, m := range msgs {
		c.ui.Notify(m, tui.LevelWarn)
	}
}
