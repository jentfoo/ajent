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
	w       *Writer
	dropped string      // first message-append failure since the last turn end
	watch   func(Entry) // optional observer of successful appends, app supplied
}

// NewRecorder returns a recorder that persists to w.
func NewRecorder(w *Writer) *Recorder { return &Recorder{w: w} }

// WatchAppend installs fn to observe each entry this recorder successfully
// appends; nil clears it. The observer fires on the appending goroutine and
// must not block or append itself.
func (r *Recorder) WatchAppend(fn func(Entry)) { r.watch = fn }

// append writes one entry through w, observing it on success.
func (r *Recorder) append(typ Type, data any) (Entry, error) {
	e, err := r.w.Append(typ, data)
	if err == nil && r.watch != nil {
		r.watch(e)
	}
	return e, err
}

// Message records one appended message. Wire as agent.Options.OnMessage. A
// failure is held for TurnEnd, which surfaces it as a notice rather than
// dropping the message silently.
func (r *Recorder) Message(info agent.MessageInfo) {
	_, err := r.append(TypeMessage, MessageData{
		Message:  info.Message,
		Stop:     info.Stop,
		Usage:    info.Usage,
		Injected: info.Injected,
		Replayed: info.Replayed,
		Origin:   info.Origin,
	})
	if err != nil && r.dropped == "" {
		r.dropped = "failed to record message: " + err.Error()
	}
}

// pullDropped returns and clears the first held append failure, if any.
func (r *Recorder) pullDropped() string {
	d := r.dropped
	r.dropped = ""
	return d
}

// Sink wraps next so notices persist and the file is fsynced at each turn end.
// Write failures surface as a notice through the wrapped sink rather than
// failing the turn, so a broken disk should not end the conversation.
func (r *Recorder) Sink(next agent.Sink) agent.Sink {
	return &recordingSink{next: next, rec: r}
}

// ModelChange records a model switch by its canonical key.
func (r *Recorder) ModelChange(m llm.Model, reason string) {
	_, _ = r.append(TypeModelChange, ModelData{Model: m.Key(), Reason: reason})
}

// Rename persists a new session name, which NameOf then reads back.
func (r *Recorder) Rename(name string) error {
	_, err := r.append(TypeSessionName, NameData{Name: name})
	return err
}

// SettingChange persists one setting value, while the caller owns any in-memory update.
func (r *Recorder) SettingChange(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = r.append(TypeSettingChange, SettingData{Key: key, Value: raw})
	return err
}

// Custom persists opaque extension state that must survive a resume.
func (r *Recorder) Custom(customType string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = r.append(TypeCustom, CustomData{CustomType: customType, Data: raw})
	return err
}

type recordingSink struct {
	next agent.Sink
	rec  *Recorder
}

func (s *recordingSink) TurnStart(i agent.TurnInfo) { s.next.TurnStart(i) }
func (s *recordingSink) UserPrompt(text string)     { s.next.UserPrompt(text) }
func (s *recordingSink) InjectContext(text string)  { s.next.InjectContext(text) }
func (s *recordingSink) Thinking(d string)          { s.next.Thinking(d) }
func (s *recordingSink) EndThinking()               { s.next.EndThinking() }
func (s *recordingSink) Text(d string)              { s.next.Text(d) }
func (s *recordingSink) EndText()                   { s.next.EndText() }

func (s *recordingSink) ToolStart(call agent.ToolCall, label string, full bool) func(agent.ToolResult) {
	return s.next.ToolStart(call, label, full)
}

func (s *recordingSink) ToolOutput(id, d string)           { s.next.ToolOutput(id, d) }
func (s *recordingSink) ToolProgress(p agent.ToolProgress) { s.next.ToolProgress(p) }
func (s *recordingSink) Diff(p, b, a string)               { s.next.Diff(p, b, a) }
func (s *recordingSink) Usage(u llm.Usage)                 { s.next.Usage(u) }
func (s *recordingSink) Context(c tokens.ContextState) {
	s.next.Context(c)
}

// Notice persists the notice then forwards it, persistence failures surfacing as
// an error-level notice through the wrapped sink.
func (s *recordingSink) Notice(msg string, level agent.Level) {
	if _, err := s.rec.append(TypeNotice, NoticeData{Message: msg, Level: level}); err != nil {
		s.next.Notice("failed to persist session: "+err.Error(), agent.LevelError)
	}
	s.next.Notice(msg, level)
}

// TurnEnd reports any held message-append failure, fsyncs the transcript at a
// turn boundary and lets failures surface as an error-level notice rather than
// ending the turn.
func (s *recordingSink) TurnEnd(r agent.TurnResult) {
	if msg := s.rec.pullDropped(); msg != "" {
		s.next.Notice(msg, agent.LevelError)
	}
	if err := s.rec.w.Sync(); err != nil {
		s.next.Notice("failed to persist session: "+err.Error(), agent.LevelError)
	}
	s.next.TurnEnd(r)
}
