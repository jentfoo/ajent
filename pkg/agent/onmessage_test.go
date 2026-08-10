package agent

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnMessageFiresOncePerMessageInOrder asserts the hook sees every appended
// message in transcript order, including user echo and tool results.
func TestOnMessageFiresOncePerMessageInOrder(t *testing.T) {
	t.Parallel()

	var seen []llm.Role
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok"}}}
	a.opts.OnMessage = func(info MessageInfo) { seen = append(seen, info.Message.Role) }

	err := a.Prompt(t.Context(), Input{Text: "run it"})
	require.NoError(t, err)

	assert.Equal(t, []llm.Role{
		llm.RoleUser, // prompt echo
		llm.RoleAssistant,
		llm.RoleUser, // tool result
		llm.RoleAssistant,
	}, seen)
}

// TestOnMessageRecordsStopReason asserts the assistant message carries the real
// stop reason: end_turn for a plain reply and aborted on an interrupt.
func TestOnMessageRecordsEndTurn(t *testing.T) {
	t.Parallel()

	var got llm.StopReason
	a := newTestAgent(nil, &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}, nil)
	a.opts.OnMessage = func(info MessageInfo) { got = info.Stop }

	require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))
	assert.Equal(t, llm.StopEndTurn, got)
}

// TestOnMessageRecordsAbortedStop asserts an interrupted partial message is
// recorded with StopAborted.
func TestOnMessageRecordsAbortedStop(t *testing.T) {
	t.Parallel()

	gp := &hangProvider{turn: textOnly("hello ")}
	a := newTestAgent(nil, gp, nil)
	catch := &resultCatcher{}
	a.opts.Sink = catch

	var got llm.StopReason
	a.opts.OnMessage = func(info MessageInfo) { got = info.Stop }

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	require.Eventually(t, func() bool { return gp.current() != nil }, defaultTimeout, pollInterval,
		"the stream must be created before cancelling")
	a.Interrupt()

	require.NoError(t, <-errCh)
	assert.Equal(t, llm.StopAborted, got)
}

// TestOnMessageRecordsToolUseStop asserts a tool-calling turn records StopToolUse
// on its assistant message.
func TestOnMessageRecordsToolUseStop(t *testing.T) {
	t.Parallel()

	toolTurn := toolCallEvents("c1", "bash")
	toolTurn[len(toolTurn)-1] = llm.Event{Type: llm.EventDone, StopReason: llm.StopToolUse}
	a := newTestAgent(nil, &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolTurn}, // assistant message stops with StopToolUse
		{Events: textOnly("")},
	}}, nil)
	a.opts.Tools = &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok"}}}

	var got []MessageInfo
	a.opts.OnMessage = func(info MessageInfo) { got = append(got, info) }

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
}

// firstBlock returns the message's first content block, or nil.
func firstBlock(m llm.Message) llm.Block {
	if len(m.Content) == 0 {
		return nil
	}
	return m.Content[0]
}
