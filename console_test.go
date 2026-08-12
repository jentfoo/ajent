package main

import (
	"os"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUIConsoleExitSignalsQuit verifies Exit closes the quit channel the
// driver watches.
func TestUIConsoleExitSignalsQuit(t *testing.T) {
	// not parallel: uses a shared channel
	c := &uiConsole{quit: make(chan struct{}, 1)}
	c.Exit()
	select {
	case <-c.quit:
	default:
		t.Fatal("Exit must signal the quit channel")
	}
}

// TestUIConsoleStartedFlag verifies Started reads the shared flag the pump owns.
func TestUIConsoleStartedFlag(t *testing.T) {
	// not parallel: mutates a shared bool
	started := false
	c := &uiConsole{started: &started}
	assert.False(t, c.Started())
	started = true
	assert.True(t, c.Started())
}

// TestUIConsoleToolsChangedNilRecorder verifies ToolsChanged is a no-op when no
// recorder is attached (recording disabled).
func TestUIConsoleToolsChangedNilRecorder(t *testing.T) {
	c := &uiConsole{tools: tools.New()}
	assert.NotPanics(t, func() { c.ToolsChanged() })
}

// TestUIConsoleSetModelRemasuresLedger verifies a mid-session /model switch does
// not leave the ledger reading empty. SetModel drops every context term for the new
// window; it must remeasure against the actual in-memory messages so switching to a
// smaller window immediately reflects real occupancy and threshold auto-compaction can fire.
func TestUIConsoleSetModelRemasuresLedger(t *testing.T) {
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

	c := &uiConsole{ui: ui, reg: reg, st: st}
	// switching to a smaller window must not leave Used at zero.
	c.SetModel(llm.Model{ID: "p/small", ContextWindow: 8000})

	assert.Greater(t, tk.Context().Used, 100,
		"a model switch must remeasure the ledger from actual messages, not read empty")
}
