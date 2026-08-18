package session

import (
	"fmt"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
)

// replaySink records every call as a short token, for sequence assertions.
type replaySink struct {
	calls []string
}

func (s *replaySink) TurnStart(i agent.TurnInfo) { s.calls = append(s.calls, "start:"+i.Input.Text) }
func (s *replaySink) Thinking(string)            { s.calls = append(s.calls, "thinking") }
func (s *replaySink) EndThinking()               { s.calls = append(s.calls, "end_thinking") }
func (s *replaySink) Text(d string)              { s.calls = append(s.calls, "text:"+d) }
func (s *replaySink) EndText()                   { s.calls = append(s.calls, "end_text") }

// ToolStart records the call and captures its completion hook.
func (s *replaySink) ToolStart(call agent.ToolCall, _ string) func(agent.ToolResult) {
	s.calls = append(s.calls, "tool:"+call.Name)
	return func(res agent.ToolResult) { s.calls = append(s.calls, fmt.Sprintf("result:%v", res.IsError)) }
}

func (s *replaySink) ToolOutput(string, string)       {}
func (s *replaySink) ToolProgress(agent.ToolProgress) {}
func (s *replaySink) Diff(string, string, string)     {}
func (s *replaySink) Usage(llm.Usage)                 {}
func (s *replaySink) Context(tokens.ContextState)     {}
func (s *replaySink) Notice(m string, _ agent.Level)  { s.calls = append(s.calls, "notice:"+m) }
func (s *replaySink) TurnEnd(agent.TurnResult)        { s.calls = append(s.calls, "turn_end") }

// replayBranch builds a minimal session branch from helper entries.
func replayBranch() []Entry {
	return []Entry{
		{ID: "s", Type: TypeSession},
		msgUser("m1", llm.Text(llm.RoleUser, "hello world")),
		assistantText("a1", "hi there"),
		noticeEntry("n1", NoticeData{Message: "careful"}),
	}
}

func msgUser(id string, m llm.Message) Entry {
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(MessageData{Message: m})}
}

func assistantText(id, text string) Entry {
	m := MessageData{
		Message: llm.Text(llm.RoleAssistant, text),
		Stop:    llm.StopEndTurn,
	}
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(m)}
}

func noticeEntry(id string, nd NoticeData) Entry {
	return Entry{ID: id, Type: TypeNotice, Data: mustJSON(nd)}
}

func assistantWithThinking(id string) Entry {
	m := MessageData{
		Message: llm.Message{Role: llm.RoleAssistant,
			Content: llm.BlockList{llm.ThinkingBlock{Text: "let me think"}, llm.TextBlock{Text: "answer"}}},
	}
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(m)}
}

func assistantWithToolCall(id string) Entry {
	m := MessageData{
		Message: llm.Message{Role: llm.RoleAssistant,
			Content: llm.BlockList{llm.ToolCallBlock{ID: "c1", Name: "bash"}}},
	}
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(m)}
}

func resultMessage(id, callID, text string) Entry {
	m := MessageData{
		Message: llm.Message{Role: llm.RoleUser,
			Content: llm.BlockList{llm.ToolResultBlock{CallID: callID, Content: llm.BlockList{llm.TextBlock{Text: text}}}}},
	}
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(m)}
}

func resultMessageErr(id, callID string) Entry {
	m := MessageData{
		Message: llm.Message{Role: llm.RoleUser,
			Content: llm.BlockList{llm.ToolResultBlock{CallID: callID, IsError: true}}},
	}
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(m)}
}

func TestReplay(t *testing.T) {
	t.Parallel()

	t.Run("condensed_sequence", func(t *testing.T) {
		s := &replaySink{}
		Replay(replayBranch(), s, ReplayOptions{})

		assert.Equal(t, []string{
			"start:hello world",
			"text:hi there", "end_text",
			"notice:careful",
			"turn_end",
		}, s.calls)
	})

	t.Run("thinking_option", func(t *testing.T) {
		branch := []Entry{
			{ID: "s", Type: TypeSession},
			msgUser("m1", llm.Text(llm.RoleUser, "q")),
			assistantWithThinking("a1"),
		}
		cases := []struct {
			name    string
			enabled bool
			wantIn  []string // expected when enabled; absent means suppressed
		}{
			{"suppressed_by_default", false, nil},
			{"present_when_enabled", true, []string{"thinking", "end_thinking"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := &replaySink{}
				Replay(branch, s, ReplayOptions{Thinking: tc.enabled})
				if len(tc.wantIn) == 0 {
					assert.NotContains(t, s.calls, "thinking")
					return
				}
				for _, w := range tc.wantIn {
					assert.Contains(t, s.calls, w)
				}
			})
		}
	})

	t.Run("tool_call_and_result", func(t *testing.T) {
		branch := []Entry{
			{ID: "s", Type: TypeSession},
			msgUser("m1", llm.Text(llm.RoleUser, "run it")),
			assistantWithToolCall("a1"),
			resultMessage("r1", "c1", "output here\nmore lines"),
		}
		s := &replaySink{}
		Replay(branch, s, ReplayOptions{})

		assert.Contains(t, s.calls, "tool:bash")
		assert.Contains(t, s.calls, "result:false") // non-error result
	})

	t.Run("tool_error_shows_status", func(t *testing.T) {
		branch := []Entry{
			{ID: "s", Type: TypeSession},
			msgUser("m1", llm.Text(llm.RoleUser, "run it")),
			assistantWithToolCall("a1"),
			resultMessageErr("r1", "c1"),
		}
		s := &replaySink{}
		Replay(branch, s, ReplayOptions{})
		assert.Contains(t, s.calls, "result:true")
	})

	t.Run("multiple_turns_close_each", func(t *testing.T) {
		branch := []Entry{
			{ID: "s", Type: TypeSession},
			msgUser("m1", llm.Text(llm.RoleUser, "first")),
			assistantText("a1", "one"),
			msgUser("m2", llm.Text(llm.RoleUser, "second")),
			assistantText("a2", "two"),
		}
		s := &replaySink{}
		Replay(branch, s, ReplayOptions{})
		assert.Equal(t,
			[]string{"start:first", "text:one", "end_text", "turn_end",
				"start:second", "text:two", "end_text", "turn_end"},
			s.calls)
	})
}
