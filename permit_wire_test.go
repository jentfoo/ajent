package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPermissionsModeDefaultResolves asserts the compiled-in default is
// allow-read and Explain reports it at the (default) layer. The home dir is
// isolated so a real user config on the machine never shifts the source.
func TestPermissionsModeDefaultResolves(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	set, _, err := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	require.NoError(t, err)
	assert.Equal(t, "allow-read", set.Settings().Permissions.Mode)

	v, src, ok := set.Explain("permissions.mode")
	require.True(t, ok)
	assert.Equal(t, `"allow-read"`, string(v))
	assert.Equal(t, "default", src)
}

// TestSetSessionSettingAppliesModeAndPublishesSegment verifies the permissions
// branch of SetSessionSetting drives the live barrier and its status segment.
func TestSetSessionSettingAppliesModeAndPublishesSegment(t *testing.T) {
	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)

	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(func() {
		ui.Close()
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})

	set, _, _ := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	b := permit.NewBarrier(func(string) bool { return false })
	c := &uiConsole{ui: ui, set: set, permit: b}

	assert.Equal(t, permit.ModeAllowRead, b.Mode())
	err = c.SetSessionSetting("permissions.mode", "block-all")
	require.NoError(t, err)
	assert.Equal(t, permit.ModeBlockAll, b.Mode())

	// recorded as a session override so Explain reports (session) and resume restores it.
	v, src, ok := set.Explain("permissions.mode")
	require.True(t, ok)
	assert.Equal(t, `"block-all"`, string(v))
	assert.Equal(t, "session", src)

	// an unparsable value leaves the barrier untouched.
	err = c.SetSessionSetting("permissions.mode", "nonsense")
	require.NoError(t, err)
	assert.Equal(t, permit.ModeBlockAll, b.Mode())
}

// TestSetSessionSettingOtherKeysLeaveBarrierAlone asserts non-permission keys do
// not disturb the barrier's mode.
func TestSetSessionSettingOtherKeysLeaveBarrierAlone(t *testing.T) {
	set, _, _ := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	b := permit.NewBarrier(func(string) bool { return false })
	c := &uiConsole{set: set, permit: b}
	err := c.SetSessionSetting("model", "p/m")
	require.NoError(t, err)
	assert.Equal(t, permit.ModeAllowRead, b.Mode())
}

// TestPromptAdapterPlainModeReportsNoUI asserts the prompter refuses in headless
// mode so a call that would prompt is denied rather than silently allowed.
func TestPromptAdapterPlainModeReportsNoUI(t *testing.T) {
	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)

	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(func() {
		ui.Close()
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})

	a := promptAdapter{ui: ui}
	_, err = a.Open("Allow?", "cmd", []string{"Allow"})
	assert.ErrorIs(t, err, tui.ErrNoUI)
}

// TestClassifierAdapterClassifiesShellCommands drives a fresh-context model call
// against a scripted provider, covering readonly/write/garbled verdicts.
func TestClassifierAdapterClassifiesShellCommands(t *testing.T) {
	t.Parallel()

	model := llm.Model{ID: "p/m", Provider: "scripted"}
	adapterFor := func(reply string) classifierAdapter {
		sp := newScripted(reply)
		return classifierAdapter{
			providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
			model:       func() llm.Model { return model },
		}
	}

	t.Run("readonly verdict", func(t *testing.T) {
		assert.Equal(t, permit.ClassReadOnly, adapterFor("read-only").Classify(context.Background(), "stat a"))
	})
	t.Run("write verdict with noise", func(t *testing.T) {
		assert.Equal(t, permit.ClassWrite, adapterFor("WRITE: it modifies the file!").Classify(context.Background(), "rm a"))
	})
	t.Run("garbled maps to unsure", func(t *testing.T) {
		assert.Equal(t, permit.ClassUnsure, adapterFor("? maybe 42").Classify(context.Background(), "weird cmd"))
	})
}

func TestClassifierAdapterNoModelIsUnsure(t *testing.T) {
	t.Parallel()

	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return nil, nil },
		model:       func() llm.Model { return llm.Model{} }, // no model configured
	}
	assert.Equal(t, permit.ClassUnsure, adapter.Classify(context.Background(), "anything"))
}

func TestClassifierAdapterProviderErrorIsUnsure(t *testing.T) {
	t.Parallel()

	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return nil, errors.New("no provider") },
		model:       func() llm.Model { return llm.Model{ID: "p/m"} },
	}
	assert.Equal(t, permit.ClassUnsure, adapter.Classify(context.Background(), "anything"))
}

func TestClassifierAdapterRequestUsesFreshContextAndMinimalReasoning(t *testing.T) {
	t.Parallel()

	model := llm.Model{ID: "p/m", Provider: "scripted", MaxOutput: 64000}
	sp := newScripted("readonly")
	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
		model:       func() llm.Model { return model },
	}

	_ = adapter.Classify(context.Background(), "stat a")
	reqs := sp.Requests()
	require.Len(t, reqs, 1)
	r := reqs[0]
	assert.Equal(t, llm.RoleUser, r.Messages[0].Role) // the command is the whole user turn
	var sys strings.Builder
	for _, b := range r.System {
		if tb, ok := b.(llm.TextBlock); ok {
			sys.WriteString(tb.Text)
		}
	}
	assert.Contains(t, sys.String(), "readonly")
	// a model that cannot reason clamps minimal to off; the field is always populated.
	assert.Equal(t, llm.ClampLevel(model, llm.LevelMinimal), r.Reasoning.Level)
}

func newScripted(reply string) *llm.ScriptedProvider {
	return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: reply},
		{Type: llm.EventTextEnd, Index: 0},
		{Type: llm.EventDone, StopReason: llm.StopEndTurn},
	}}}}
}

// TestGuardRegisteredAgainstRegistry verifies the barrier's guard and asker are
// registered so an unverifiable call asks through them.
func TestGuardRegisteredAgainstRegistry(t *testing.T) {
	reg := tools.New()
	b := permit.NewBarrier(func(string) bool { return false })
	reg.AddGuard(b.Guard())
	assert.NotPanics(t, func() { reg.SetAsker(b.Asker()) })

	tool := &stubTool{name: "write"}
	reg.RegisterState("builtin", tool, tools.StateEnabled)
	g, ok := reg.Get("write")
	require.True(t, ok)

	res, err := g.Execute(context.Background(), agent.ToolCall{
		ID: "c1", Name: "write", Input: []byte(`{}`),
	}, nil)
	require.NoError(t, err) // a denial is an error result, not a Go error
	assert.True(t, res.IsError)
}

// stubTool is a minimal agent.Tool for guard-chain tests.
type stubTool struct{ name string }

func (s *stubTool) Name() string                { return s.name }
func (s *stubTool) Label(agent.ToolCall) string { return s.name + " ..." }
func (s *stubTool) Description() string         { return "test tool" }
func (s *stubTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: s.name} }
func (s *stubTool) Mode() agent.ExecutionMode   { return agent.ModeSerial }
func (s *stubTool) Execute(_ context.Context, _ agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}
