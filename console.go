package main

import (
	"context"

	"github.com/jentfoo/ajent/pkg/mcp"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// uiConsole implements command.Console over the objects the driver already
// holds: the UI, model registry, agent state, tool registry and session
// recorder. Keeping it here means main.go's bespoke /model and /reasoning
// switch move into pkg/command without a cycle.
type uiConsole struct {
	ui       *tui.UI
	set      *config.Set
	reg      *llm.Registry
	st       *agent.State
	tools    *tools.Registry
	commands *command.Registry
	rec      *session.Recorder
	comp     *compactor // nil when session recording is off
	mcp      mcpAdapter // the MCP server manager adapter, or nil

	started *bool // shared with the driver pump
	quit    chan struct{}
}

func (c *uiConsole) Notify(msg string, level tui.Level) { c.ui.Notify(msg, level) }
func (c *uiConsole) Print(markdown string)              { c.ui.Print(markdown) }

func (c *uiConsole) Pick(ctx context.Context, prompt string, items []tui.PickItem, opts tui.PickOptions) (int, error) {
	return c.ui.PickContext(ctx, prompt, items, opts)
}

func (c *uiConsole) MultiPick(ctx context.Context, prompt string, items []tui.PickItem, opts tui.MultiPickOptions) ([]int, error) {
	return c.ui.MultiPickContext(ctx, prompt, items, opts)
}

func (c *uiConsole) Select(ctx context.Context, prompt string, options []tui.Option) (int, error) {
	return c.ui.SelectContext(ctx, prompt, options)
}

func (c *uiConsole) Confirm(ctx context.Context, prompt string) (bool, error) {
	return c.ui.ConfirmContext(ctx, prompt)
}

func (c *uiConsole) Input(ctx context.Context, label, placeholder string) (string, error) {
	return c.ui.InputContext(ctx, label, placeholder)
}

func (c *uiConsole) Models() *llm.Registry       { return c.reg }
func (c *uiConsole) State() *agent.State         { return c.st }
func (c *uiConsole) Tools() *tools.Registry      { return c.tools }
func (c *uiConsole) MCP() command.MCPServers     { return c.mcp }
func (c *uiConsole) Commands() *command.Registry { return c.commands }
func (c *uiConsole) Settings() *config.Set       { return c.set }

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

// SetSessionSetting applies a dotted key as a session override and records it so
// a resume restores it, mirroring ToolsChanged.
func (c *uiConsole) SetSessionSetting(key string, value any) error {
	if c.set != nil {
		if err := c.set.SetSession(key, value); err != nil {
			return err
		}
	}
	if c.rec != nil {
		return c.rec.SettingChange(key, value)
	}
	return nil
}

func (c *uiConsole) SetModel(m llm.Model) {
	c.reg.SetActive(m)
	c.st.Model = m
	// rebase the ledger's window and reserve onto the new model so a mid-session
	// /model rescales the bar immediately rather than on the next turn.
	t := c.st.Tokens
	if t != nil {
		t.SetModel(m) // drops every context term for the new window/reserve
	}
	c.ui.SetModel(m.Key(), m.ContextWindow)
	if t != nil {
		// SetModel zeroed the ledger, so Used would read empty regardless of real
		// occupancy. Remeasure against the actual in-memory messages: a switch to a
		// smaller window must reflect that it now overflows, or threshold auto-compaction
		// could never fire on this model.
		t.Reseed(tokens.EstimateMessages(c.st.Messages))
		cs := t.Context()
		c.ui.SetContext(tui.ContextInfo{
			Used:      cs.Used,
			Window:    cs.Window,
			Reserve:   cs.Reserve,
			Compact:   cs.Compact,
			Estimated: cs.Estimated,
		})
	}
	// record a session override so Explain and Settings report (session).
	if c.set != nil {
		_ = c.set.SetSession("model", m.Key())
	}
	c.ui.Notify("model: "+m.Key(), tui.LevelInfo)
	if c.rec != nil {
		c.rec.ModelChange(m, "command")
	}
}

func (c *uiConsole) SetReasoning(rc llm.ReasoningConfig) {
	c.st.Reasoning = rc
	// persist granular dotted leaves so every choice survives: marshalling the
	// whole config would drop retain "none" and show false under omitempty.
	if c.set != nil {
		_ = c.set.SetSession("reasoning.level", rc.Level.String())
		_ = c.set.SetSession("reasoning.show", rc.Show)
		_ = c.set.SetSession("reasoning.retain", rc.Retain.String())
	}
	// keep the status indicator in step with a non-default level.
	c.ui.SetStatusSegment(tui.Segment{Key: "reasoning", Text: levelOrEmpty(rc)})
	c.ui.Notify("reasoning: "+rc.Level.String(), tui.LevelInfo)
	if c.rec != nil {
		_ = c.rec.SettingChange("reasoning", rc)
	}
}

func (c *uiConsole) ToolsChanged() {
	if c.tools == nil {
		return
	}
	// the dotted config key; applySetting still accepts the legacy "tools" alias
	names := c.tools.Names()
	if c.set != nil {
		_ = c.set.SetSession("tools.enabled", names)
	}
	if c.rec != nil {
		_ = c.rec.SettingChange("tools.enabled", names)
	}
	// /tools changed which MCP tools are enabled; republish the status ratio
	if c.mcp.m != nil {
		c.mcp.RefreshStatus()
	}
}

func (c *uiConsole) Started() bool { return *c.started }

// Compact reduces the session context toward the compaction threshold. An empty
// instructions string runs an unguided pass; refused while a turn streams.
func (c *uiConsole) Compact(ctx context.Context, instructions string) error {
	if c.comp == nil {
		c.ui.Notify("compaction needs an open session", tui.LevelWarn)
		return nil
	}
	_, err := c.comp.run(ctx, agent.CompactManual, instructions)
	return err
}

func (c *uiConsole) Exit() {
	select {
	case c.quit <- struct{}{}:
	default:
	}
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
			Connected: s.Connected,
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

// Groups maps the manager's tool groups onto /tools headers.
func (a mcpAdapter) Groups() []command.MCPGroup {
	src := a.m.ToolGroups()
	out := make([]command.MCPGroup, len(src))
	for i, g := range src {
		out[i] = command.MCPGroup{Source: g.Source, Label: g.Label}
	}
	return out
}

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
