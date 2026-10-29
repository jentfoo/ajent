package app

import (
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
)

// ctlStreamSink republishes every agent turn event onto the control wire, one
// event per sink call, using the json one-shot's shapes plus tool.result. It
// registers through the hub fanout, after the recorder.
type ctlStreamSink struct {
	agent.NopSink
	hub *sessionHub

	mu     sync.Mutex
	output map[string]*strings.Builder // streamed tool output by call id, tool.result fallback
}

func (s *ctlStreamSink) TurnStart(i agent.TurnInfo) {
	s.hub.publish(ctl.EventTurnStart, ctl.TurnStartData{Model: i.Model.Key()})
}

func (s *ctlStreamSink) UserPrompt(text string) {
	s.hub.publish(ctl.EventUserPrompt, ctl.UserPromptData{Text: text})
}

func (s *ctlStreamSink) Thinking(d string) {
	s.hub.publish(ctl.EventThinkingDelta, ctl.TextData{Delta: d})
}

func (s *ctlStreamSink) EndThinking() {
	s.hub.publish(ctl.EventThinkingEnd, nil)
}

func (s *ctlStreamSink) Text(d string) {
	s.hub.publish(ctl.EventTextDelta, ctl.TextData{Delta: d})
}

func (s *ctlStreamSink) EndText() {
	s.hub.publish(ctl.EventTextEnd, nil)
}

func (s *ctlStreamSink) ToolStart(call agent.ToolCall, label string, full bool) func(agent.ToolResult) {
	s.hub.publish(ctl.EventToolStart, ctl.ToolStartData{
		ID: call.ID, Name: call.Name, Input: call.Input, Label: label, Full: full,
	})
	return func(res agent.ToolResult) {
		staged := s.takeOutput(call.ID) // always consumed, so streamed output never accumulates
		out := res.Display
		if strings.TrimSpace(out) == "" {
			out = staged
		}
		if strings.TrimSpace(out) == "" {
			out = llm.FirstBlockText(res.Content)
		}
		s.hub.publish(ctl.EventToolResult, ctl.ToolResultData{
			ID: call.ID, Name: call.Name, Output: out, Error: res.IsError,
		})
	}
}

func (s *ctlStreamSink) ToolOutput(id, d string) {
	s.hub.publish(ctl.EventToolOutput, ctl.ToolOutputData{ID: id, Delta: d})

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.output == nil {
		s.output = make(map[string]*strings.Builder)
	}
	b, ok := s.output[id]
	if !ok {
		b = &strings.Builder{}
		s.output[id] = b
	}
	b.WriteString(d) // amortized: streamed output can reach megabytes
}

// takeOutput consumes one call's streamed output, the json one-shot's
// tool_result fallback order.
func (s *ctlStreamSink) takeOutput(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.output[id]
	delete(s.output, id)
	if b == nil {
		return ""
	}
	return b.String()
}

func (s *ctlStreamSink) ToolProgress(p agent.ToolProgress) {
	s.hub.publish(ctl.EventToolProgress, ctl.ToolProgressData{
		ID: p.CallID, Name: p.Name, Path: p.Path, Bytes: p.Bytes, Lines: p.Lines, Done: p.Done,
	})
}

func (s *ctlStreamSink) Diff(p, b, a string) {
	s.hub.publish(ctl.EventDiff, ctl.DiffData{Path: p, Before: b, After: a})
}

func (s *ctlStreamSink) Usage(u llm.Usage) {
	s.hub.publish(ctl.EventUsage, ctlUsage(u))
}

func (s *ctlStreamSink) Context(c tokens.ContextState) {
	s.hub.publish(ctl.EventContext, ctl.ContextData{
		Used: c.Used, Window: c.Window, Reserve: c.Reserve, Compact: c.Compact, Estimated: c.Estimated,
	})
}

func (s *ctlStreamSink) Notice(msg string, level agent.Level) {
	s.hub.publish(ctl.EventNotice, ctl.NoticeData{Text: msg, Level: levelName(level)})
}

func (s *ctlStreamSink) TurnEnd(r agent.TurnResult) {
	d := ctl.TurnEndData{Stop: r.Stop.String(), Steps: r.Steps, Usage: ctlUsage(r.Usage)}
	if r.Err != nil {
		d.Err = r.Err.Error()
	}
	s.hub.publish(ctl.EventTurnEnd, d)
}

// controlEmitters builds the hub observer hooks republishing each rendered
// surface as its control event. One emitter per phase 33 surface, not a second
// status implementation.
func controlEmitters(hub *sessionHub) hubEmitters {
	return hubEmitters{
		Echo: func(text string) {
			hub.publish(ctl.EventEcho, ctl.EchoData{Text: text})
		},
		EchoImages: func(imgs []tui.Image) {
			hub.publish(ctl.EventEchoImages, ctl.EchoImagesData{Images: ctlImages(imgs)})
		},
		Notice: func(key, msg string, level tui.Level) {
			hub.publish(ctl.EventNotice, ctl.NoticeData{Text: msg, Level: levelName(agent.Level(level)), Key: key})
		},
		Print: func(markdown string) {
			hub.publish(ctl.EventPrint, ctl.PrintData{Markdown: markdown})
		},
		Segment: func(seg tui.Segment) {
			hub.publish(ctl.EventStatus, ctl.StatusData{Segment: &ctl.Segment{
				Key: seg.Key, Text: seg.Text, Short: seg.Short, Order: seg.Order,
			}})
		},
		Model: func(key, short string, window int) {
			hub.publish(ctl.EventStatus, ctl.StatusData{Model: &ctl.ModelState{
				Key: key, Short: short, Window: window,
			}})
		},
		Spinning: func(on bool) {
			hub.publish(ctl.EventStatus, ctl.StatusData{Spinning: &on})
		},
		Activity: func(key, text string, rank int) {
			hub.publish(ctl.EventActivity, ctl.ActivityData{Key: key, Text: text, Rank: rank})
		},
		Queued: func(texts []string) {
			hub.publish(ctl.EventQueued, ctl.QueuedData{Texts: texts})
		},
		Draft: func(text string) {
			hub.publish(ctl.EventEditor, ctl.EditorData{Text: text})
		},
		Head: func(id string) {
			hub.publish(ctl.EventHead, ctl.HeadData{ID: id})
		},
	}
}

// controlDialogHooks republishes broker announcements: pending on Begin,
// settled on End, so no client holds a dialog that never resolves.
func controlDialogHooks(hub *sessionHub) (func(interactionInfo), func(uint64, interactionOutcome)) {
	begin := func(info interactionInfo) {
		hub.publish(ctl.EventInteractionPending, ctl.InteractionPendingData{
			ID: info.ID, Kind: info.Kind.String(), Prompt: info.Prompt,
			Subject: info.Subject, Options: info.Shapes.options,
			Multi: info.Shapes.multi, Text: info.Shapes.text,
			Secret: info.Shapes.secret, Chat: info.Shapes.chat,
		})
	}
	end := func(id uint64, o interactionOutcome) {
		hub.publish(ctl.EventInteractionSettled, ctl.InteractionSettledData{
			ID: id, Kind: o.Kind.String(), Answered: o.Answered, Cancelled: o.Cancelled,
			Index: o.Index, Indexes: o.Indexes, Text: o.Text, Chat: o.Chat,
		})
	}
	return begin, end
}

// --- conversions from hub and session types onto the wire ---

// ctlStateFrom maps one hub snapshot onto the wire state.
func ctlStateFrom(s hubSnapshot) ctl.State {
	st := ctl.State{
		Session:     ctl.Session{ID: s.SessionID, Name: s.SessionName},
		Workspace:   s.Workspace,
		Cwd:         s.Cwd,
		Head:        s.Head,
		Model:       ctlStateModel(s.Model),
		Permission:  s.Permission,
		Running:     s.Running,
		Idle:        s.Idle,
		Spinning:    s.Spinning,
		Queued:      s.Queued,
		QueuedTexts: s.QueuedTexts,
		Context: ctl.ContextData{Used: s.Context.Used, Window: s.Context.Window,
			Reserve: s.Context.Reserve, Compact: s.Context.Compact, Estimated: s.Context.Estimated},
		Usage: ctl.UsageState{Turns: s.Usage.Turns, Total: ctlUsage(s.Usage.Total),
			Child: ctlUsage(s.Usage.Child), ByModel: ctlUsageByModel(s.Usage.ByModel),
			Estimated: s.Usage.Estimated},
		Status: ctl.StatusRow{
			Spinner:  s.Status.Spinner,
			Segments: ctlSegments(s.Status.Segments),
		},
	}
	if s.Tools != nil {
		st.Tools = make([]ctl.ToolState, len(s.Tools))
		for i, t := range s.Tools {
			st.Tools[i] = ctl.ToolState{Name: t.Name, Source: t.Source, Enabled: t.Enabled, ReadOnly: t.ReadOnly}
		}
	}
	if s.Activity != nil {
		st.Activity = make([]ctl.ActivityRow, len(s.Activity))
		for i, a := range s.Activity {
			st.Activity[i] = ctl.ActivityRow{Key: a.Key, Text: a.Text, Rank: a.Rank}
		}
	}
	return st
}

func ctlStateModel(m hubModel) ctl.ModelState {
	return ctl.ModelState{
		Key: m.Key, Short: m.Short, Window: m.Window,
		Caps: ctl.Caps{Reasoning: m.Caps.Reasoning, Images: m.Caps.Images,
			ParallelToolCalls: m.Caps.ParallelTools},
	}
}

func ctlUsage(u llm.Usage) ctl.UsageData {
	return ctl.UsageData{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead,
		CacheWrite: u.CacheWrite, Reasoning: u.Reasoning}
}

func ctlUsageByModel(byModel map[string]llm.Usage) map[string]ctl.UsageData {
	out := make(map[string]ctl.UsageData, len(byModel))
	for k, u := range byModel {
		out[k] = ctlUsage(u)
	}
	return out
}

func ctlSegments(segs []tui.Segment) []ctl.Segment {
	out := make([]ctl.Segment, len(segs))
	for i, s := range segs {
		out[i] = ctl.Segment{Key: s.Key, Text: s.Text, Short: s.Short, Order: s.Order}
	}
	return out
}

func ctlImages(imgs []tui.Image) []ctl.ImageData {
	out := make([]ctl.ImageData, len(imgs))
	for i, im := range imgs {
		out[i] = ctl.ImageData{Data: im.Data, MediaType: im.MediaType, Label: im.Label, W: im.W, H: im.H}
	}
	return out
}

func ctlEntry(e session.Entry) ctl.Entry {
	return ctl.Entry{ID: e.ID, ParentID: e.ParentID, Type: string(e.Type), TS: e.TS, Data: e.Data}
}
