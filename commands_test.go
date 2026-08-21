package main

import (
	"context"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterCommands guards the built-in set against being displaced by a
// feature that registers its own commands: an earlier plan-workflow attempt
// replaced the RegisterBuiltins call and silently lost /help, /model and the
// rest.
func TestRegisterCommands(t *testing.T) {
	t.Parallel()

	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
	console := &uiConsole{reg: reg, st: &agent.State{}, tools: tools.New()}
	baseline := command.NewRegistry()
	command.RegisterBuiltins(baseline, console)
	require.NotEmpty(t, baseline.Names())

	t.Run("builtins_only", func(t *testing.T) {
		cmds := command.NewRegistry()
		registerCommands(cmds, console)
		assert.Equal(t, baseline.Names(), cmds.Names())
	})

	t.Run("builtins_survive_extras", func(t *testing.T) {
		cmds := command.NewRegistry()
		registerCommands(cmds, console,
			command.Command{Name: "plan", Handler: noopHandler},
			command.Command{Name: "plan-stop", Handler: noopHandler},
			command.Command{Name: "plan-status", Handler: noopHandler},
		)
		names := cmds.Names()
		assert.Subset(t, names, baseline.Names())
		assert.Subset(t, names, []string{"plan", "plan-stop", "plan-status"})
	})
}

func noopHandler(context.Context, string, command.Console) error { return nil }

func TestTurnRecorderLast(t *testing.T) {
	t.Parallel()

	var rec turnRecorder
	assert.Equal(t, agent.TurnResult{}, rec.last()) // nothing ended yet

	rec.TurnEnd(agent.TurnResult{Stop: llm.StopAborted, Steps: 2})
	assert.Equal(t, llm.StopAborted, rec.last().Stop)
	assert.Equal(t, 2, rec.last().Steps)

	rec.TurnEnd(agent.TurnResult{Stop: llm.StopEndTurn})
	assert.Equal(t, llm.StopEndTurn, rec.last().Stop) // latest wins
}

func TestLastAssistantText(t *testing.T) {
	t.Parallel()

	assistant := func(text string) llm.Message {
		return llm.Message{Role: llm.RoleAssistant,
			Content: llm.BlockList{llm.TextBlock{Text: text}}}
	}

	t.Run("newest_assistant_wins", func(t *testing.T) {
		msgs := []llm.Message{
			llm.Text(llm.RoleUser, "kickoff"),
			assistant("first"),
			llm.Text(llm.RoleUser, "more"),
			assistant("second"),
		}
		assert.Equal(t, "second", lastAssistantText(msgs))
	})

	t.Run("skips_empty_replies", func(t *testing.T) {
		msgs := []llm.Message{assistant("real answer"), assistant("   ")}
		assert.Equal(t, "real answer", lastAssistantText(msgs))
	})

	t.Run("joins_text_blocks", func(t *testing.T) {
		msgs := []llm.Message{{Role: llm.RoleAssistant, Content: llm.BlockList{
			llm.TextBlock{Text: "one "}, llm.TextBlock{Text: "two"}}}}
		assert.Equal(t, "one two", lastAssistantText(msgs))
	})

	t.Run("no_assistant_is_empty", func(t *testing.T) {
		assert.Empty(t, lastAssistantText([]llm.Message{llm.Text(llm.RoleUser, "hi")}))
		assert.Empty(t, lastAssistantText(nil))
	})
}
