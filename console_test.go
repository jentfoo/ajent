package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
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

// TestUIConsoleToolsChangedWritesThrough verifies a /tools change persists the
// enabled names into session settings, and is safe with no wiring attached.
func TestUIConsoleToolsChangedWritesThrough(t *testing.T) {
	t.Parallel()

	reg := tools.New()
	reg.Register(&stubTool{name: "read"}, true)
	reg.Register(&stubTool{name: "bash"}, false)

	// unwired console must not panic on the no-op path.
	assert.NotPanics(t, func() { (&uiConsole{tools: reg}).ToolsChanged() })

	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)
	c := &uiConsole{tools: reg, set: set}

	assert.NotPanics(t, func() { c.ToolsChanged() })

	// the enabled names land in session settings.
	raw, layer, ok := set.Explain("tools.enabled")
	require.True(t, ok)
	assert.Equal(t, "session", layer)
	assert.JSONEq(t, `["read"]`, string(raw))
}

// TestUIConsoleSetModelRemasuresLedger verifies a mid-session /model switch does
// not leave the ledger reading empty. SetModel drops every context term for the new
// window; it must remeasure against the actual in-memory messages so switching to a
// smaller window immediately reflects real occupancy and threshold auto-compaction can fire.
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

	c := &uiConsole{ui: ui, reg: reg, st: st}
	// switching to a smaller window must not leave Used at zero.
	c.SetModel(llm.Model{ID: "p/small", ContextWindow: 8000})

	assert.Greater(t, tk.Context().Used, 100,
		"a model switch must remeasure the ledger from actual messages, not read empty")
}

// TestUIConsoleSetModelNoChangeSilent verifies re-selecting the already-active
// model is a no-op: it neither rebases state nor records an announcement line, so
// a stray /model on the current model stays quiet. A real change still records.
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

	// a real recorder so we can assert what does (and does not) get recorded.
	trPath := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := session.Create(trPath, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	c := &uiConsole{ui: ui, reg: reg, st: st, rec: session.NewRecorder(w)}

	// re-selecting the same model must not record a change.
	c.SetModel(active)
	assert.NotContains(t, readFileString(t, trPath), "model_change",
		"same-model SetModel must be a silent no-op")

	// selecting a different model still records the switch.
	c.SetModel(llm.Model{Provider: "p", ID: "other"})
	assert.Contains(t, readFileString(t, trPath), "model_change",
		"a real change must record a model_change entry")
}

// readFileString returns a file's contents as text for assertions.
func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}
