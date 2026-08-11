package session

import (
	"encoding/json"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// Recorder bridges an agent turn stream onto a Writer. The Message hook is the
// durability path: one entry per message, written as the loop appends it, so a
// crash mid-turn resumes with its tool results intact.
type Recorder struct {
	w *Writer
}

// NewRecorder returns a recorder that persists to w.
func NewRecorder(w *Writer) *Recorder { return &Recorder{w: w} }

// Message records one appended message. Wire as agent.Options.OnMessage.
func (r *Recorder) Message(info agent.MessageInfo) {
	_, _ = r.w.Append(TypeMessage, MessageData{Message: info.Message, Stop: info.Stop, Usage: info.Usage})
}

// Sink wraps next so notices persist and the file is fsynced at each turn end.
// Write failures surface as a notice through the wrapped sink rather than
// failing the turn — a broken disk should not end the conversation.
func (r *Recorder) Sink(next agent.Sink) agent.Sink {
	return &recordingSink{next: next, rec: r}
}

// ModelChange records a model switch by its canonical key.
func (r *Recorder) ModelChange(m llm.Model, reason string) {
	_, _ = r.w.Append(TypeModelChange, ModelData{Model: m.Key(), Reason: reason})
}

// SettingChange persists one setting value; the caller owns any in-memory update.
func (r *Recorder) SettingChange(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = r.w.Append(TypeSettingChange, SettingData{Key: key, Value: raw})
	return err
}

// Custom persists opaque extension state that must survive a resume.
func (r *Recorder) Custom(customType string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = r.w.Append(TypeCustom, CustomData{CustomType: customType, Data: raw})
	return err
}

type recordingSink struct {
	next agent.Sink
	rec  *Recorder
}

func (s *recordingSink) TurnStart(i agent.TurnInfo) { s.next.TurnStart(i) }
func (s *recordingSink) Thinking(d string)          { s.next.Thinking(d) }
func (s *recordingSink) EndThinking()               { s.next.EndThinking() }
func (s *recordingSink) Text(d string)              { s.next.Text(d) }
func (s *recordingSink) EndText()                   { s.next.EndText() }

func (s *recordingSink) ToolStart(call agent.ToolCall, label string) func(agent.ToolResult) {
	return s.next.ToolStart(call, label)
}

func (s *recordingSink) ToolOutput(id, d string) { s.next.ToolOutput(id, d) }
func (s *recordingSink) Diff(p, b, a string)     { s.next.Diff(p, b, a) }
func (s *recordingSink) Usage(u llm.Usage)       { s.next.Usage(u) }
func (s *recordingSink) Context(c tokens.ContextState) {
	s.next.Context(c)
}

// Notice persists the notice, then forwards it; persistence failures surface as
// an error-level notice through the wrapped sink.
func (s *recordingSink) Notice(msg string, level agent.Level) {
	if _, err := s.rec.w.Append(TypeNotice, NoticeData{Message: msg, Level: level}); err != nil {
		s.next.Notice("failed to persist session: "+err.Error(), agent.LevelError)
	}
	s.next.Notice(msg, level)
}

// TurnEnd fsyncs the transcript at a turn boundary.
func (s *recordingSink) TurnEnd(r agent.TurnResult) {
	_ = s.rec.w.Sync()
	s.next.TurnEnd(r)
}
