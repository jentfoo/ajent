package app

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reloadModelsJSON = `{"providers":{"anthropic":{"apiKeyEnv":"TEST_API_KEY","models":[
	{"id":"alpha","contextWindow":100000},{"id":"beta","contextWindow":200000}]}}}`

// newReloadConsole builds a console over a temp AJENT_HOME holding the given
// config.json body, plus a two-model models.json.
func newReloadConsole(t *testing.T, cfg string) *uiConsole {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "models.json"), []byte(reloadModelsJSON), 0o600))

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	go func() { _, _ = io.Copy(io.Discard, outR) }() // notices must never fill the pipe
	t.Cleanup(func() {
		ui.Close()
		_ = inR.Close()
		_ = inW.Close()
		_ = outW.Close()
		_ = outR.Close()
	})

	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)
	file, _, err := llm.LoadUserFile()
	require.NoError(t, err)
	reg, _ := llm.NewRegistry(file, nil, llm.RegistryOptions{})
	active, err := reg.Resolve("anthropic/alpha")
	require.NoError(t, err)

	st := &agent.State{Model: active, Tokens: tokens.New(active)}
	return &uiConsole{
		ui: wrapUI(t, ui), set: set, reg: reg, st: st, tools: tools.New(),
		lane:   newStateLane(),
		q:      newSteerQueue(nil, nil, nil, nil, nil), // offer/stopDrain fake a live drain
		starts: newStartState(true),                    // the base block is committed, refreshBase applies
		reload: reloadDeps{startup: startupKeysOf(set.Settings())},
	}
}

// writeUserConfig replaces the console's config.json on disk.
func writeUserConfig(t *testing.T, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("AJENT_HOME"), "config.json"), []byte(body), 0o600))
}

// writeUserModels replaces the console's models.json on disk.
func writeUserModels(t *testing.T, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("AJENT_HOME"), "models.json"), []byte(body), 0o600))
}

func TestUIConsoleReload(t *testing.T) {
	// not parallel: t.Setenv pins AJENT_HOME for every layer path

	t.Run("applies_config_edit", func(t *testing.T) {
		c := newReloadConsole(t, `{"reasoning":{"level":"low"}}`)
		c.st.Reasoning = llm.ReasoningFrom(c.set.Settings().Reasoning, c.st.Model)
		require.Equal(t, llm.LevelLow, c.st.Reasoning.Level)

		writeUserConfig(t, `{"reasoning":{"level":"high"}}`)
		require.NoError(t, c.Reload(t.Context()))
		assert.Equal(t, llm.LevelHigh, c.st.Reasoning.Level)
	})

	t.Run("adopts_config_model", func(t *testing.T) {
		c := newReloadConsole(t, `{"model":"anthropic/alpha"}`)

		writeUserConfig(t, `{"model":"anthropic/beta"}`)
		require.NoError(t, c.Reload(t.Context()))
		assert.Equal(t, "anthropic/beta", c.st.Model.Key())
	})

	t.Run("adopts_widened_window", func(t *testing.T) {
		c := newReloadConsole(t, `{}`) // no config model: the registry's default stays live

		writeUserModels(t, `{"providers":{"anthropic":{"apiKeyEnv":"TEST_API_KEY","models":[
			{"id":"alpha","contextWindow":400000}]}}}`)
		require.NoError(t, c.Reload(t.Context()))
		assert.Equal(t, 400000, c.st.Model.ContextWindow) // agent state rebases too
		assert.Equal(t, 400000, c.reg.Active().ContextWindow)
	})

	t.Run("applies_images_block", func(t *testing.T) {
		c := newReloadConsole(t, `{}`)
		require.True(t, tools.ImagesEnabled())

		writeUserConfig(t, `{"images":{"block":true}}`)
		require.NoError(t, c.Reload(t.Context()))
		assert.False(t, tools.ImagesEnabled())
	})

	t.Run("keeps_session_model", func(t *testing.T) {
		c := newReloadConsole(t, `{"model":"anthropic/alpha"}`)
		require.NoError(t, c.set.SetSession("model", "anthropic/alpha"))

		writeUserConfig(t, `{"model":"anthropic/beta"}`)
		require.NoError(t, c.Reload(t.Context()))
		assert.Equal(t, "anthropic/alpha", c.st.Model.Key()) // the session override still wins
	})

	t.Run("tools_enabled_not_applied", func(t *testing.T) {
		c := newReloadConsole(t, `{}`)
		c.tools.Register(&stubTool{name: "read"}, true)
		c.tools.Register(&stubTool{name: "bash"}, false)

		writeUserConfig(t, `{"tools":{"enabled":["read","bash"]}}`)
		require.NoError(t, c.Reload(t.Context()))
		assert.Equal(t, []string{"read"}, c.tools.Names()) // only /tools may widen
	})

	t.Run("warns_startup_key_once", func(t *testing.T) {
		c := newReloadConsole(t, `{}`)
		require.Zero(t, c.reload.startup.maxSteps)

		writeUserConfig(t, `{"agent":{"maxSteps":40}}`)
		require.NoError(t, c.Reload(t.Context()))
		assert.Equal(t, 40, c.reload.startup.maxSteps) // reported, so a later reload stays quiet

		require.NoError(t, c.Reload(t.Context()))
		assert.Equal(t, 40, c.reload.startup.maxSteps)
	})

	t.Run("applies_compaction_threshold", func(t *testing.T) {
		c := newReloadConsole(t, `{}`)

		writeUserConfig(t, `{"compaction":{"threshold":0.5}}`)
		require.NoError(t, c.Reload(t.Context()))
		assert.InDelta(t, 0.5, c.st.Model.CompactThreshold, 0) // agent state rebases too
	})

	t.Run("refused_while_running", func(t *testing.T) {
		c := newReloadConsole(t, `{"reasoning":{"level":"low"}}`)
		c.st.Reasoning = llm.ReasoningFrom(c.set.Settings().Reasoning, c.st.Model)
		c.reload.running = func() bool { return true }

		writeUserConfig(t, `{"reasoning":{"level":"high"}}`)
		require.NoError(t, c.Reload(t.Context()))
		assert.Equal(t, llm.LevelLow, c.st.Reasoning.Level)
	})

	t.Run("queues_write_behind_drain", func(t *testing.T) {
		c := newReloadConsole(t, `{"reasoning":{"level":"low"}}`)
		c.st.Reasoning = llm.ReasoningFrom(c.set.Settings().Reasoning, c.st.Model)
		c.q.offer(agent.Input{Text: "run"}, "run", 0) // a turn starts during the reads

		writeUserConfig(t, `{"reasoning":{"level":"high"}}`)
		require.NoError(t, c.Reload(t.Context()))
		assert.Equal(t, llm.LevelLow, c.st.Reasoning.Level) // deferred behind the drain

		c.q.stopDrain() // the drain exits and the pump lands what reload queued
		c.applyDeferred()
		assert.Equal(t, llm.LevelHigh, c.st.Reasoning.Level)
	})

	t.Run("broken_config_keeps_previous", func(t *testing.T) {
		c := newReloadConsole(t, `{"reasoning":{"level":"low"}}`)
		c.st.Reasoning = llm.ReasoningFrom(c.set.Settings().Reasoning, c.st.Model)

		writeUserConfig(t, `{"reasoning":`)
		require.Error(t, c.Reload(t.Context()))
		assert.Equal(t, llm.LevelLow, c.st.Reasoning.Level)
	})
}

func TestStartupKeysChanged(t *testing.T) {
	t.Parallel()

	base := startupKeys{render: "auto", color: "auto", maxSteps: 10, maxConcurrent: 4}
	assert.Empty(t, base.changed(base))

	moved := startupKeys{render: "alt", color: "none", images: "kitty",
		keyNewline: []string{"ctrl+j"}, maxSteps: 20, turnRetries: 2,
		systemPrompt: "be brief", shellCommands: []string{"rg"}, diffEcho: "never",
		maxConcurrent: 8, subMaxSteps: 50, subPollWait: "10m", control: true}
	assert.Equal(t, []string{"ui.render", "ui.color", "ui.keymap.newline", "ui.images",
		"agent.maxSteps", "agent.turnRetries", "agent.systemPrompt",
		"tools.shellCommands", "tools.diffEcho", "subagent.maxConcurrent",
		"subagent.maxSteps", "subagent.maxPollWait", "control.enabled"},
		base.changed(moved))

	assert.Equal(t, []string{"agent.maxSteps"},
		base.changed(startupKeys{render: "auto", color: "auto", maxSteps: 20, maxConcurrent: 4}))

	assert.Equal(t, []string{"permissions.safeCommands"},
		base.changed(startupKeys{render: "auto", color: "auto", maxSteps: 10, maxConcurrent: 4,
			safe: []string{"git diff"}}))
}
