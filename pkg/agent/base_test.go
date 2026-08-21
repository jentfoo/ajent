package agent

import (
	"context"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
)

func TestAgentBaseEstimate(t *testing.T) {
	t.Parallel()

	t.Run("system_only", func(t *testing.T) {
		a := newTestAgent(nil, &llm.ScriptedProvider{}, nil)
		assert.Positive(t, a.BaseEstimate(false))
	})
	t.Run("with_tools", func(t *testing.T) {
		st := &State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}}
		a := newTestAgent(st, &llm.ScriptedProvider{}, nil)
		sysOnly := a.BaseEstimate(false)

		set := &mapSet{tools: map[string]Tool{
			"read": fakeRead{},
		}}
		a.opts.Tools = set
		assert.Greater(t, a.BaseEstimate(true), sysOnly)
	})
	t.Run("zero_while_running", func(t *testing.T) {
		st := &State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}}
		a := newTestAgent(st, &llm.ScriptedProvider{}, nil)
		a.mu.Lock()
		a.running = true
		a.mu.Unlock()
		assert.Zero(t, a.BaseEstimate(false))
	})
	t.Run("includes_project_instructions", func(t *testing.T) {
		st := &State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}}
		a := newTestAgent(st, &llm.ScriptedProvider{}, nil)
		bare := a.BaseEstimate(false)

		a.opts.ProjectInstructions = []ProjectInstruction{{Body: "the entire AGENTS.md body rides in the system block"}}
		assert.Greater(t, a.BaseEstimate(false), bare)
	})
}

// fakeRead is a minimal tool so mapSet has something to expose schemas for.
type fakeRead struct{}

func (fakeRead) Name() string          { return "read" }
func (fakeRead) Label(ToolCall) string { return "read" }
func (fakeRead) Description() string   { return "reads a file" }
func (fakeRead) Schema() llm.ToolSchema {
	return llm.ToolSchema{Name: "read", Parameters: []byte(`{}`)}
}
func (fakeRead) Mode() ExecutionMode { return ModeParallel }
func (fakeRead) Execute(context.Context, ToolCall, Output) (ToolResult, error) {
	return ToolResult{}, nil
}
