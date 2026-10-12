package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

func TestUIConsoleToolsChangedWritesThrough(t *testing.T) {
	t.Parallel()

	reg := tools.New()
	reg.Register(&stubTool{name: "read"}, true)
	reg.Register(&stubTool{name: "bash"}, false)

	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)
	started := newStartedFlag(false)
	c := &uiConsole{tools: reg, set: set, ag: agent.New(&agent.State{}, agent.Options{}), started: started, refreshBase: func() {}}

	assert.NotPanics(t, func() { c.ToolsChanged() })

	// the enabled names land in session settings
	raw, layer, ok := set.Explain("tools.enabled")
	require.True(t, ok)
	assert.Equal(t, "session", layer)
	assert.JSONEq(t, `["read"]`, string(raw))
}

func TestUIConsoleSetModelRemasuresLedger(t *testing.T) {
	t.Parallel()

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

	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
	st := &agent.State{
		Model:  llm.Model{ID: "p/big", ContextWindow: 200000},
		Tokens: tokens.New(llm.Model{ID: "p/big", ContextWindow: 8000}),
	}
	// a nontrivial in-memory context exists before the switch.
	st.Messages = []llm.Message{
		llm.Text(llm.RoleUser, strings.Repeat("x ", 500)), // ~1000+ chars of prose
	}
	tk := st.Tokens
	tk.Add(tokens.EstimateMessages(st.Messages)) // the live ledger already tracks them
	require.Greater(t, tk.Context().Used, 1)

	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)
	c := &uiConsole{ui: ui, set: set, reg: reg, st: st, ag: agent.New(st, agent.Options{})}
	// switching to a smaller window must not leave Used at zero.
	c.SetModel(llm.Model{ID: "p/small", ContextWindow: 8000})

	assert.Greater(t, tk.Context().Used, 100)
}

func TestUIConsoleSetModelNoChangeSilent(t *testing.T) {
	t.Parallel()

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

	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
	active := llm.Model{Provider: "p", ID: "same"}
	st := &agent.State{
		Model:  active,
		Tokens: tokens.New(active),
	}

	// a real recorder so we can assert what does (and does not) get recorded
	trPath := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := session.Create(trPath, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)
	c := &uiConsole{ui: ui, set: set, reg: reg, st: st, rec: session.NewRecorder(w), ag: agent.New(st, agent.Options{})}

	// re-selecting the same model must not record a change.
	c.SetModel(active)
	assert.NotContains(t, readFileString(t, trPath), "model_change")

	// selecting a different model still records the switch.
	c.SetModel(llm.Model{Provider: "p", ID: "other"})
	assert.Contains(t, readFileString(t, trPath), "model_change")
}

func TestUIConsoleSetModelKeepsSessionOnly(t *testing.T) {
	// not parallel: t.Setenv pins AJENT_HOME for the user layer path

	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)
	userCfg := filepath.Join(home, "config.json")

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

	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)
	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
	active := llm.Model{Provider: "p", ID: "first"}
	st := &agent.State{Model: active, Tokens: tokens.New(active)}
	c := &uiConsole{ui: ui, set: set, reg: reg, st: st, ag: agent.New(st, agent.Options{})}

	// the same-model no-op must not create the file.
	c.SetModel(active)
	assert.NoFileExists(t, userCfg)

	c.SetModel(llm.Model{Provider: "p", ID: "picked"})

	// the change lands as a session override only, so no config layer is written.
	assert.NoFileExists(t, userCfg)
	rv, src, ok := set.Explain("model")
	require.True(t, ok)
	assert.Equal(t, "session", src)
	assert.JSONEq(t, `"p/picked"`, string(rv))
}

func TestUIConsoleSetModelPreservesReasoningIntent(t *testing.T) {
	// not parallel: t.Setenv pins AJENT_HOME for the user layer path

	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)

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

	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)
	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
	reasoning := llm.Model{
		Provider: "p", ID: "claude",
		Caps: llm.Capabilities{Reasoning: true},
	}
	st := &agent.State{Model: reasoning, Tokens: tokens.New(reasoning)}
	c := &uiConsole{ui: ui, set: set, reg: reg, st: st, ag: agent.New(st, agent.Options{})}

	// the user's explicit choice persists as a session override (mirrors /reasoning).
	c.SetReasoning(llm.ReasoningConfig{Level: llm.LevelHigh})

	// switching to a model that cannot reason clamps only the live effective level.
	c.SetModel(llm.Model{Provider: "p", ID: "small"})
	assert.Equal(t, llm.LevelOff, st.Reasoning.Level)
	rl, layer, ok := set.Explain("reasoning.level")
	require.True(t, ok)
	assert.Equal(t, "session", layer)
	assert.JSONEq(t, `"high"`, string(rl)) // stored intent survives

	// switching back restores the user's original choice.
	c.SetModel(reasoning)
	assert.Equal(t, llm.LevelHigh, st.Reasoning.Level)
}

// TestUIConsoleMutatorsRefusedMidTurn pins a turn inside its tool call and
// checks that the state mutators refuse instead of racing the loop goroutine.
func TestUIConsoleMutatorsRefusedMidTurn(t *testing.T) {
	t.Parallel()

	ui := newPumpTestUI(t)
	release := make(chan struct{})
	entered := make(chan struct{})

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: mainToolTurn()},
		{Events: textTurn("done")},
	}}
	st := &agent.State{Model: llm.Model{ID: "big", Provider: "p"}, Tokens: tokens.New(llm.Model{ID: "big", Provider: "p"})}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Sinks:    []agent.Sink{agent.NopSink{}},
		Tools:    singleToolSet{tool: holdBash{release: release, entered: entered}},
		Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})

	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)
	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
	c := &uiConsole{ui: ui, set: set, reg: reg, st: st, ag: ag}

	q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {})
	started := newStartedFlag(false)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var drain drainWaiter
	startDrain(ctx, ui, false, ag, q, []agent.Input{{Text: "run"}}, started, planHooks{}, &drain)

	<-entered // the turn owns State from here until it settles

	modelBefore := st.Model.Key()
	reasoningBefore := st.Reasoning
	c.SetModel(llm.Model{Provider: "p", ID: "small"})
	assert.Equal(t, modelBefore, st.Model.Key())       // refused, nothing moved
	assert.NotEqual(t, "session", set.Source("model")) // and nothing was recorded
	c.SetReasoning(llm.ReasoningConfig{Level: llm.LevelHigh})
	assert.Equal(t, reasoningBefore, st.Reasoning)

	close(release) // unpin the tool call so the turn can end
	drain.wait(5 * time.Second)

	c.SetModel(llm.Model{Provider: "p", ID: "small"}) // idle again, applies now
	assert.Equal(t, "p/small", st.Model.Key())
}

// readFileString returns a file's contents as text for assertions.
func readFileString(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func TestUIConsoleSetSessionSettingAppliesCompactThreshold(t *testing.T) {
	t.Parallel()

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

	f := llm.File{Providers: map[string]llm.ProviderConfig{
		"p": {Flavor: llm.FlavorAnthropic, Models: []llm.ModelConfig{
			{ID: "big", ContextWindow: ptrTo(200000)},
		}},
	}}
	reg, _ := llm.NewRegistry(f, nil, llm.RegistryOptions{})
	model, err := reg.Resolve("p/big")
	require.NoError(t, err)

	st := &agent.State{Model: model, Tokens: tokens.New(model)}
	st.Messages = []llm.Message{llm.Text(llm.RoleUser, strings.Repeat("x ", 500))}
	tk := st.Tokens
	tk.Add(tokens.EstimateMessages(st.Messages))
	used := tk.Context().Used
	require.Greater(t, used, 1)

	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)
	c := &uiConsole{ui: ui, set: set, reg: reg, st: st, ag: agent.New(st, agent.Options{})}
	require.NoError(t, c.SetSessionSetting("compaction.threshold", 0.5))

	assert.InDelta(t, 0.5, c.st.Model.CompactThreshold, 1e-09)
	assert.Equal(t, 100000, tk.Context().Compact)
	assert.Equal(t, used, tk.Context().Used)
}

// Serial: flips the package-wide images gate. The apply must read the value
// just set, not the stale one, so each flip lands immediately.
func TestUIConsoleImagesBlockLiveFlip(t *testing.T) {
	set := loadTestConfig(t)
	tools.SetImagesEnabled(false)
	t.Cleanup(func() { tools.SetImagesEnabled(true) })

	c := &uiConsole{set: set}
	require.NoError(t, c.SetSessionSetting("images.block", false))
	assert.True(t, tools.ImagesEnabled())
	require.NoError(t, c.SetSessionSetting("images.block", true))
	assert.False(t, tools.ImagesEnabled())
}

func TestUIConsolePermissionModeAppliesButNotRecorded(t *testing.T) {
	t.Parallel()

	reg := tools.New()
	reg.Register(&stubTool{name: "read"}, true)
	barrier := permit.NewBarrier(reg.ReadOnly, permit.Options{})
	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)

	c := &uiConsole{set: set, permit: barrier}
	require.Equal(t, "allow-read", barrier.Mode().String())

	require.NoError(t, c.SetSessionSetting("permissions.mode", "auto"))
	assert.Equal(t, "auto", barrier.Mode().String()) // applied live
}

func ptrTo[T any](v T) *T { return &v }
