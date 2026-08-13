package main

import (
	"context"

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
	c.ui.SetStatusSegment("reasoning", levelOrEmpty(rc))
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

// levelOrEmpty returns the reasoning segment text, or "" at the default level.
func levelOrEmpty(rc llm.ReasoningConfig) string {
	if rc.Level == llm.LevelMedium {
		return ""
	}
	return rc.Level.String()
}
