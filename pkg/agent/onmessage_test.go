package agent

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOnMessage(t *testing.T) {
	t.Parallel()

	// the hook sees every appended message in transcript order, including user echo and tool results.
	t.Run("fires_once_per_message_in_order", func(t *testing.T) {
		var seen []llm.Role
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: toolCallEvents("c1", "bash")},
			{Events: textOnly("done")},
		}}
		a := newTestAgent(nil, p, nil)
		a.opts.Tools = &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok"}}}
		a.opts.OnMessage = []func(MessageInfo){func(info MessageInfo) { seen = append(seen, info.Message.Role) }}

		err := a.Prompt(t.Context(), Input{Text: "run it"})
		require.NoError(t, err)

		assert.Equal(t, []llm.Role{
			llm.RoleUser, // prompt echo
			llm.RoleAssistant,
			llm.RoleUser, // tool result
			llm.RoleAssistant,
		}, seen)
	})

	// the assistant message carries end_turn for a plain reply.
	t.Run("records_end_turn", func(t *testing.T) {
		var got llm.StopReason
		a := newTestAgent(nil, &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}, nil)
		a.opts.OnMessage = []func(MessageInfo){func(info MessageInfo) { got = info.Stop }}

		require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))
		assert.Equal(t, llm.StopEndTurn, got)
	})

	// an interrupted partial message is recorded with StopAborted.
	t.Run("records_aborted_stop", func(t *testing.T) {
		gp := &hangProvider{turn: textOnly("hello ")}
		catch := &resultCatcher{}
		a := newTestAgent(nil, gp, catch)

		var got llm.StopReason
		a.opts.OnMessage = []func(MessageInfo){func(info MessageInfo) { got = info.Stop }}

		errCh := make(chan error, 1)
		go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
		require.Eventually(t, func() bool { return gp.current() != nil }, defaultTimeout, pollInterval,
			"the stream must be created before cancelling")
		a.Interrupt()

		require.NoError(t, <-errCh)
		assert.Equal(t, llm.StopAborted, got)
	})

	// a tool-calling turn records StopToolUse on its assistant message.
	t.Run("records_tool_use_stop", func(t *testing.T) {
		toolTurn := toolCallEvents("c1", "bash")
		toolTurn[len(toolTurn)-1] = llm.Event{Type: llm.EventDone, StopReason: llm.StopToolUse}
		a := newTestAgent(nil, &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: toolTurn}, // assistant message stops with StopToolUse
			{Events: textOnly("")},
		}}, nil)
		a.opts.Tools = &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok"}}}

		var got []MessageInfo
		a.opts.OnMessage = []func(MessageInfo){func(info MessageInfo) { got = append(got, info) }}

		require.NoError(t, a.Prompt(t.Context(), Input{Text: "run"}))
		// the assistant tool-call message carries StopToolUse; user messages carry none
		var found bool
		for _, info := range got {
			if _, ok := firstBlock(info.Message).(llm.ToolCallBlock); !ok {
				continue // only the tool-call assistant message carries StopToolUse
			}
			assert.Equal(t, llm.StopToolUse, info.Stop)
			found = true
		}
		require.True(t, found)
	})
}

// firstBlock returns the message's first content block, or nil.
func firstBlock(m llm.Message) llm.Block {
	if len(m.Content) == 0 {
		return nil
	}
	return m.Content[0]
}
