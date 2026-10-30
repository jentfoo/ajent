package session

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
)

// replaySink records every call as a short token, for sequence assertions.
type replaySink struct {
	calls  []string
	labels []string
}

func (s *replaySink) TurnStart(i agent.TurnInfo) { s.calls = append(s.calls, "start:"+i.Input.Text) }
func (s *replaySink) UserPrompt(text string)     { s.calls = append(s.calls, "user:"+text) }
func (s *replaySink) InjectContext(string)       {}
func (s *replaySink) Thinking(string)            { s.calls = append(s.calls, "thinking") }
func (s *replaySink) EndThinking()               { s.calls = append(s.calls, "end_thinking") }
func (s *replaySink) Text(d string)              { s.calls = append(s.calls, "text:"+d) }
func (s *replaySink) EndText()                   { s.calls = append(s.calls, "end_text") }

// ToolStart records the call and captures its completion hook. The label is kept
// separately so an injected block, which carries no tool name, is still checkable.
func (s *replaySink) ToolStart(call agent.ToolCall, label string, _ bool) func(agent.ToolResult) {
	s.labels = append(s.labels, label)
	s.calls = append(s.calls, "tool:"+call.Name)
	return func(res agent.ToolResult) {
		s.calls = append(s.calls, fmt.Sprintf("result:%v|%s", res.IsError, res.Display))
	}
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

// parallelResults builds one assistant message with two tool calls and a single
// user result message resolving both, mirroring a parallel dispatch turn.
func parallelBranch() []Entry {
	return []Entry{
		{ID: "s", Type: TypeSession},
		msgUser("m1", llm.Text(llm.RoleUser, "inspect")),
		assistantToolCalls("a1", llm.ToolCallBlock{ID: "c1", Name: "bash"}, llm.ToolCallBlock{ID: "c2", Name: "read"}),
		resultMessagePair("r1", []llm.Block{
			llm.ToolResultBlock{CallID: "c1", Content: llm.BlockList{llm.TextBlock{Text: "ls out"}}},
			llm.ToolResultBlock{CallID: "c2", Content: llm.BlockList{llm.TextBlock{Text: "go mod body"}}},
		}),
	}
}

func assistantToolCalls(id string, calls ...llm.Block) Entry {
	return Entry{ID: id, Type: TypeMessage,
		Data: mustJSON(MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: calls}})}
}

func resultMessagePair(id string, blocks []llm.Block) Entry {
	return Entry{ID: id, Type: TypeMessage,
		Data: mustJSON(MessageData{Message: llm.Message{Role: llm.RoleUser, Content: blocks}})}
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

func injectedUser(id, text string) Entry {
	return Entry{ID: id, Type: TypeMessage,
		Data: mustJSON(MessageData{
			Injected: true,
			Message:  llm.Text(llm.RoleUser, text),
		})}
}

func replayedUser(id, text string) Entry {
	return Entry{ID: id, Type: TypeMessage,
		Data: mustJSON(MessageData{
			Injected: true,
			Replayed: true,
			Message:  llm.Text(llm.RoleUser, text),
		})}
}

func TestReplay(t *testing.T) {
	t.Parallel()

	t.Run("condensed_sequence", func(t *testing.T) {
		s := &replaySink{}
		Replay(replayBranch(), s, ReplayOptions{})

		assert.Equal(t, []string{
			"start:hello world", "user:hello world",
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
			wantIn  []string // expected when enabled, absent means suppressed
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

	t.Run("tool_result_renders_header_and_body", func(t *testing.T) {
		cases := []struct {
			name string
			res  Entry
			want [4]string
		}{
			{"success_shows_output",
				resultMessage("r1", "c1", "output here\nmore lines"),
				[4]string{"start:run it", "user:run it", "tool:bash", "result:false|output here\nmore lines"}},
			// an erroring call still renders its header above the status
			{"error_shows_status",
				resultMessageErr("r1", "c1"),
				[4]string{"start:run it", "user:run it", "tool:bash", "result:true|"}},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				branch := []Entry{
					{ID: "s", Type: TypeSession},
					msgUser("m1", llm.Text(llm.RoleUser, "run it")),
					assistantWithToolCall("a1"),
					tc.res,
				}
				s := &replaySink{}
				Replay(branch, s, ReplayOptions{})
				assert.Equal(t, tc.want[:], s.calls[:4])
			})
		}
	})

	t.Run("parallel_calls_interleave_headers_and_bodies", func(t *testing.T) {
		s := &replaySink{}
		Replay(parallelBranch(), s, ReplayOptions{})

		assert.Equal(t,
			[]string{
				"start:inspect", "user:inspect",
				"tool:bash", "result:false|ls out", // header above its own body
				"tool:read", "result:false|go mod body",
			},
			s.calls[:6])
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
			[]string{"start:first", "user:first", "text:one", "end_text", "turn_end",
				"start:second", "user:second", "text:two", "end_text", "turn_end"},
			s.calls)
	})

	// a message the reader tolerated as JSON but whose payload no longer decodes
	// must not let later replies fold into the previous turn's window
	t.Run("decode_error_closes_turn", func(t *testing.T) {
		branch := []Entry{
			{ID: "s", Type: TypeSession},
			msgUser("m1", llm.Text(llm.RoleUser, "first")),
			assistantText("a1", "one"),
			{ID: "bad", Type: TypeMessage, Data: json.RawMessage(`"torn"`)},
			assistantText("a2", "two"),
		}
		s := &replaySink{}
		Replay(branch, s, ReplayOptions{})
		// the reply after corruption opens no turn of its own (its prompt is lost),
		// but it must not close the window opened before it either
		assert.Equal(t,
			[]string{"start:first", "user:first", "text:one", "end_text", "turn_end",
				"text:two", "end_text"},
			s.calls)
	})

	t.Run("injected_user_message_skipped", func(t *testing.T) {
		branch := []Entry{
			{ID: "s", Type: TypeSession},
			msgUser("m1", llm.Text(llm.RoleUser, "real prompt")),
			assistantText("a1", "one"),
			injectedUser("i1", "survey context"), // machinery, carrying no replay mark
		}
		s := &replaySink{}
		Replay(branch, s, ReplayOptions{})
		// unmarked injected context neither opens a turn nor draws anything
		assert.Equal(t, []string{"start:real prompt", "user:real prompt", "text:one", "end_text", "turn_end"}, s.calls)
	})

	t.Run("replayed_injected_draws_block", func(t *testing.T) {
		branch := []Entry{
			{ID: "s", Type: TypeSession},
			replayedUser("i1", "User Ran: echo hi\n\nOutput:\none"), // staged `!` result
			msgUser("m1", llm.Text(llm.RoleUser, "real prompt")),
			assistantText("a1", "one"),
		}
		s := &replaySink{}
		Replay(branch, s, ReplayOptions{})

		// the staged run draws above the prompt it fed, as its own block rather
		// than a turn of its own: first line labels it, the rest is its body
		assert.Equal(t, []string{
			"tool:", "result:false|Output:\none",
			"start:real prompt", "user:real prompt", "text:one", "end_text", "turn_end",
		}, s.calls)
		assert.Equal(t, []string{"User Ran: echo hi"}, s.labels)
	})

	t.Run("replayed_injected_blank_label_skipped", func(t *testing.T) {
		branch := []Entry{
			{ID: "s", Type: TypeSession},
			replayedUser("i1", "   "),
			msgUser("m1", llm.Text(llm.RoleUser, "real prompt")),
		}
		s := &replaySink{}
		Replay(branch, s, ReplayOptions{})
		assert.Equal(t, []string{"start:real prompt", "user:real prompt", "turn_end"}, s.calls)
	})
}

// echoSink records only the standalone shapes EchoEntry may paint, ignoring
// the turn machinery and real tool headers Replay folds, so a parity run
// compares like with like.
type echoSink struct {
	agent.NopSink
	calls []string
}

func (s *echoSink) UserPrompt(text string) { s.calls = append(s.calls, "user:"+text) }
func (s *echoSink) Notice(m string, l agent.Level) {
	s.calls = append(s.calls, fmt.Sprintf("notice:%d:%s", l, m))
}

// ToolStart records a real tool header only when the call names a tool; an
// injected block carries none, which is what distinguishes it from folding.
func (s *echoSink) ToolStart(call agent.ToolCall, label string, _ bool) func(agent.ToolResult) {
	if call.Name != "" {
		return func(agent.ToolResult) {} // replay folding, not an echo shape
	}
	s.calls = append(s.calls, "iblock:"+label)
	return func(res agent.ToolResult) { s.calls = append(s.calls, "ibody:"+res.Display) }
}

// TestEchoEntry pins the standalone paint rule entry by entry: a typed prompt
// echoes, a replay-marked injected block draws as its own block, a notice
// replays, and everything Replay folds or the stream owns paints nothing.
func TestEchoEntry(t *testing.T) {
	t.Parallel()

	prompt := msgUser("m1", llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
		llm.TextBlock{Text: "one"},
		llm.TextBlock{Text: "two"},
	}})
	cases := []struct {
		name string
		e    Entry
		want []string
	}{
		{"prompt_joins_text", prompt, []string{"user:one two"}},
		{"tool_result_carrier", resultMessage("r1", "c1", "denied by user"), nil},
		{"parallel_result_carrier", parallelBranch()[2], nil},
		{"error_result_carrier", resultMessageErr("r2", "c1"), nil},
		{"injected_context", injectedUser("i1", "User Ran: date"), nil},
		{"replayed_injected_draws_block", replayedUser("i2", "User Ran: date\n\nOutput:\n16:00"),
			[]string{"iblock:User Ran: date", "ibody:Output:\n16:00"}},
		{"replayed_injected_blank_label", replayedUser("i3", "   "), nil},
		{"assistant_message", assistantText("a1", "answer"), nil},
		{"notice_entry", noticeEntry("n1", NoticeData{Message: "careful"}), []string{"notice:0:careful"}},
		{"session_entry", Entry{ID: "s", Type: TypeSession}, nil},
		{"undecodable_data", Entry{ID: "x", Type: TypeMessage, Data: []byte("{")}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &echoSink{}
			EchoEntry(tc.e, s)
			assert.Equal(t, tc.want, s.calls)
		})
	}
}

// TestEchoEntryMatchesReplay pins the coupling between live rendering and
// replay: for every entry of a branch carrying all message shapes, EchoEntry
// paints exactly the standalone calls Replay draws for that entry, in order.
// Replay may change how it folds turns and tools; this test is what forces the
// live echo rule to follow.
func TestEchoEntryMatchesReplay(t *testing.T) {
	t.Parallel()

	branch := []Entry{
		{ID: "s", Type: TypeSession},
		msgUser("m1", llm.Text(llm.RoleUser, "first prompt")),
		assistantToolCalls("a1", llm.ToolCallBlock{ID: "c1", Name: "bash"}),
		resultMessage("r1", "c1", "ls out"),
		msgUser("m2", llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
			llm.TextBlock{Text: "steering"},
			llm.ToolResultBlock{CallID: "c2", Content: llm.BlockList{llm.TextBlock{Text: "kept"}}},
		}}),
		injectedUser("i1", "User Ran: date"),
		replayedUser("i2", "User Ran: echo hi"),
		replayedUser("i3", "   "),
		msgUser("m3", llm.Text(llm.RoleUser, "last prompt")),
		noticeEntry("n1", NoticeData{Message: "careful"}),
		assistantText("a2", "done"),
		{ID: "bad", Type: TypeMessage, Data: json.RawMessage(`"torn"`)},
	}

	full := &echoSink{}
	Replay(branch, full, ReplayOptions{})

	per := make([]string, 0, len(full.calls)) // parity: same calls, same order
	for _, e := range branch {
		s := &echoSink{}
		EchoEntry(e, s)
		per = append(per, s.calls...)
	}
	assert.Equal(t, []string{
		"user:first prompt",
		"user:steering",
		"iblock:User Ran: echo hi", "ibody:", // no body: the label is the whole text
		"user:last prompt",
		"notice:0:careful",
	}, full.calls)
	assert.Equal(t, full.calls, per)
}
