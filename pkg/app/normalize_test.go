package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
)

func TestSteerNormalizesThroughRefs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644))
	reg, err := tools.Builtins(tools.Options{Cwd: dir})
	require.NoError(t, err)

	block := make(chan struct{})
	set := &singleToolSet{tool: holdBash{release: block}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: mainToolTurn()},
		{Events: textTurnRewind("after steer")},
	}}

	expander := refs.NewExpander(reg, agent.NopSink{}, tools.PathPolicy{Cwd: dir}, nil)
	st := &agent.State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}, Tokens: tokens.New(llm.Model{ID: "test"})}
	a := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Sinks:    []agent.Sink{agent.NopSink{}},
		Tools:    set,
		NormalizeInput: func(in agent.Input) agent.Input {
			return refs.Normalize(expander, in, func(string) {})
		},
	})

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), agent.Input{Text: "start"}) }()
	require.Eventually(t, func() bool { return a.Running() }, 2*time.Second, time.Millisecond)

	// a host-supplied steer bypassing the pump, with a raw @ reference
	require.True(t, a.Steer(agent.Input{Text: "look at @a.go"}))
	close(block)
	require.NoError(t, <-errCh)

	reqs := p.Requests()
	require.Len(t, reqs, 2)
	msgs := reqs[1].Messages

	// the steered message keeps its literal text and gains its read pair behind it
	var text, callID string
	var sawResult bool
	for _, m := range msgs {
		for _, b := range m.Content {
			switch blk := b.(type) {
			case llm.TextBlock:
				if blk.Text == "look at @a.go" {
					text = blk.Text
				}
			case llm.ToolCallBlock:
				if blk.Name == "read" {
					callID = blk.ID
				}
			case llm.ToolResultBlock:
				sawResult = sawResult || (blk.CallID == callID && callID != "")
			}
		}
	}
	assert.Equal(t, "look at @a.go", text)
	assert.NotEmpty(t, callID)
	assert.True(t, sawResult)
}
