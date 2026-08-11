package session

import (
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureSink records the notices and turn ends it receives.
type captureSink struct {
	notices []string
	turnEnd int
}

func (c *captureSink) TurnStart(agent.TurnInfo) {}
func (c *captureSink) Thinking(string)          {}
func (c *captureSink) EndThinking()             {}
func (c *captureSink) Text(string)              {}
func (c *captureSink) EndText()                 {}
func (c *captureSink) ToolStart(agent.ToolCall, string) func(agent.ToolResult) {
	return func(agent.ToolResult) {}
}
func (c *captureSink) ToolOutput(string, string)        {}
func (c *captureSink) Diff(string, string, string)      {}
func (c *captureSink) Usage(llm.Usage)                  {}
func (c *captureSink) Context(tokens.ContextState)      {}
func (c *captureSink) Notice(msg string, _ agent.Level) { c.notices = append(c.notices, msg) }
func (c *captureSink) TurnEnd(agent.TurnResult)         { c.turnEnd++ }

// countMessages returns the number of message entries in a file.
func countMessages(t *testing.T, p string) int {
	t.Helper()
	entries, _, err := Read(p)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if e.Type == TypeMessage {
			n++
		}
	}
	return n
}

func TestRecorderOneEntryPerMessage(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	r := NewRecorder(w)

	for range 3 {
		r.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "hi"), Stop: llm.StopEndTurn})
	}
	assert.Equal(t, 3, countMessages(t, p))
}

func TestRecorderSinkPersistsNoticesAndSyncsOnTurnEnd(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	r := NewRecorder(w)

	caps := &captureSink{}
	s := r.Sink(caps)
	s.Notice("hello", agent.LevelWarn)
	assert.Equal(t, []string{"hello"}, caps.notices)

	entries, _, _ := Read(p)
	var nd NoticeData
	require.NoError(t, entries[len(entries)-1].Decode(&nd))
	assert.Equal(t, "hello", nd.Message)
	assert.Equal(t, agent.LevelWarn, nd.Level)

	s.TurnEnd(agent.TurnResult{Stop: llm.StopEndTurn})
	assert.Equal(t, 1, caps.turnEnd)

	// durability path survives a reopen after the turn boundary
	w2, oerr := Open(p)
	require.NoError(t, oerr)
	defer func() { _ = w2.Close() }()
	assert.NotEmpty(t, w2.Head())
}

func TestRecorderWriteFailureNoticesInsteadOfFailing(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	// close the fd directly so Appends fail while f stays non-nil
	require.NoError(t, w.f.Close())

	r := NewRecorder(w)
	caps := &captureSink{}
	s := r.Sink(caps)

	s.Notice("hello", agent.LevelInfo) // persistence fails; a notice surfaces instead
	assert.Contains(t, caps.notices[0], "failed to persist session")
	// the original notice still forwards so nothing is lost from the UI
	assert.Equal(t, "hello", caps.notices[1])
}

func TestRecorderSettingAndCustomRoundTrip(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	r := NewRecorder(w)

	require.NoError(t, r.SettingChange("tools", []string{"read"}))
	require.NoError(t, r.Custom("plan", map[string]any{"step": 1}))

	entries, _, _ := Read(p)
	assert.Equal(t, TypeSettingChange, entries[1].Type)
	var sd SettingData
	require.NoError(t, entries[1].Decode(&sd))
	assert.JSONEq(t, `["read"]`, string(sd.Value))

	var cd CustomData
	require.NoError(t, entries[2].Decode(&cd))
	assert.Equal(t, "plan", cd.CustomType)
}

func TestRecorderModelChangePersistsKey(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	r := NewRecorder(w)

	r.ModelChange(llm.Model{Provider: "anthropic", ID: "claude"}, "manual")

	entries, _, _ := Read(p)
	var md ModelData
	require.NoError(t, entries[1].Decode(&md))
	assert.Equal(t, "anthropic/claude", md.Model)
}
