package app

import (
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tui"
)

// ctlStreamSink republishes every agent turn event onto the control wire, one
// event per sink call, using the json one-shot's shapes plus tool.result. It
// registers through the hub fanout, after the recorder.
//
// Context, Usage and Notice stay no-ops: the hub owns the context event,
// per-turn usage rides turn.end with ledger totals covered by snapshots, and
// recorded notices reach clients as their entry.append transcript entries.
type ctlStreamSink struct {
	// NopSink stands in for UserPrompt and InjectContext: committed entries are
	// the only form those take on the socket, and every attached surface paints
	// each one exactly once through the commit/echo rule.
	agent.NopSink
	hub *sessionHub

	mu     sync.Mutex
	output map[string]*strings.Builder // streamed tool output by call id, tool.result fallback
}

func (s *ctlStreamSink) TurnStart(i agent.TurnInfo) {
	s.hub.publish(ctl.EventTurnStart, ctl.TurnStartData{Model: i.Model.Key()})
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
		s.hub.publish(ctl.EventToolResult, ctl.ToolResultData{
			ID: call.ID, Name: call.Name,
			Output: toolResultBody(res.Display, staged, res.Content), Error: res.IsError,
		})
	}
}

// toolResultBody picks a finished call's body for the wire and json lines:
// what history shows when it differs from the streamed output, then streamed
// output no one else saw, then the model-facing content. The apply side of the
// same convention lives on the applier's toolResult.
func toolResultBody(display, streamed string, blocks llm.BlockList) string {
	if strings.TrimSpace(display) != "" {
		return display
	}
	if strings.TrimSpace(streamed) != "" {
		return streamed
	}
	return llm.FirstBlockText(blocks)
}

func (s *ctlStreamSink) ToolOutput(id, d string) {
	s.hub.publish(ctl.EventToolOutput, ctl.ToolOutputData{ID: id, Delta: d})

	// nothing streams with no listener: the staged copy only feeds tool.result's
	// streamed fallback, and a mid-call joiner reads display or content instead
	if s.hub.evLive.Load() == 0 {
		return
	}
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

func (s *ctlStreamSink) TurnEnd(r agent.TurnResult) {
	d := ctl.TurnEndData{Stop: r.Stop.String(), Steps: r.Steps, Usage: ctlUsage(r.Usage)}
	if r.Err != nil {
		d.Err = r.Err.Error()
	}
	s.hub.publish(ctl.EventTurnEnd, d)
}

// controlDialogHooks republishes broker announcements: pending on Begin,
// settled on End, so no client holds a dialog that never resolves.
func controlDialogHooks(hub *sessionHub) (func(interactionInfo), func(uint64, interactionOutcome)) {
	begin := func(info interactionInfo) {
		hub.publish(ctl.EventInteractionPending, ctlInteraction(info))
	}
	end := func(id uint64, o interactionOutcome) {
		hub.publish(ctl.EventInteractionSettled, ctl.InteractionSettledData{
			ID: id, Kind: o.Kind.String(), Answered: o.Answered, Cancelled: o.Cancelled,
			Index: o.Index, Indexes: o.Indexes, Text: o.Text, Chat: o.Chat,
		})
	}
	return begin, end
}

// ctlInteraction maps one dialog announcement onto its wire shape: the
// pending event and the snapshot's interaction list share it exactly, so a
// resnapshot says what a pending event would say.
func ctlInteraction(info interactionInfo) ctl.InteractionPendingData {
	return ctl.InteractionPendingData{
		ID: info.ID, Kind: info.Kind.String(), Prompt: info.Prompt,
		Subject: info.Subject, Options: info.Shapes.options,
		Multi: info.Shapes.multi, Text: info.Shapes.text,
		Secret: info.Shapes.secret, Chat: info.Shapes.chat,
	}
}

// --- conversions from hub and session types onto the wire ---

// ctlStateFrom maps one hub snapshot onto the wire state.
func ctlStateFrom(s hubSnapshot) ctl.State {
	st := ctl.State{
		Session:    ctl.Session{ID: s.SessionID, Name: s.SessionName},
		Workspace:  s.Workspace,
		Cwd:        s.Cwd,
		Head:       s.Head,
		Model:      ctlStateModel(s.Model),
		Permission: s.Permission,
		Running:    s.Running,
		Idle:       s.Idle,
		Spinning:   s.Spinning,
		Queued:     s.Queued,
		QueuedRows: s.QueuedRows,
		Context:    contextWire(s.Context),
		Usage: ctl.UsageState{Turns: s.Usage.Turns, Total: ctlUsage(s.Usage.Total),
			Child: ctlUsage(s.Usage.Child), ByModel: ctlUsageByModel(s.Usage.ByModel),
			Estimated: s.Usage.Estimated},
	}
	if s.Status.Segments != nil {
		st.Status.Segments = make([]ctl.Segment, len(s.Status.Segments))
		for i, seg := range s.Status.Segments {
			st.Status.Segments[i] = ctlSegment(seg)
		}
	}
	if s.Interactions != nil {
		st.Interactions = make([]ctl.InteractionPendingData, len(s.Interactions))
		for i, info := range s.Interactions {
			st.Interactions[i] = ctlInteraction(info)
		}
	}
	if s.Tools != nil {
		st.Tools = ctlTools(s.Tools)
	}
	if s.Activity != nil {
		st.Activity = make([]ctl.ActivityRow, len(s.Activity))
		for i, a := range s.Activity {
			st.Activity[i] = ctl.ActivityRow{Key: a.Key, Text: a.Text, Rank: a.Rank}
		}
	}
	return st
}

// ctlSegment maps one view-model segment onto the wire shape.
func ctlSegment(seg tui.Segment) ctl.Segment {
	return ctl.Segment{Key: seg.Key, Text: seg.Text, Short: seg.Short,
		Order: seg.Order, Counts: seg.Counts}
}

func ctlStateModel(m hubModel) ctl.ModelState {
	return ctl.ModelState{
		Key: m.Key, Short: m.Short, Window: m.Window,
		Caps: ctl.Caps{Reasoning: m.Caps.Reasoning, Images: m.Caps.Images,
			ParallelToolCalls: m.Caps.ParallelTools},
	}
}

// ctlTools maps the view model's tool rows onto the wire, the shape both the
// snapshot and the tools event carry.
func ctlTools(rows []hubTool) []ctl.ToolState {
	out := make([]ctl.ToolState, len(rows))
	for i, t := range rows {
		out[i] = ctl.ToolState{Name: t.Name, Source: t.Source, Enabled: t.Enabled, ReadOnly: t.ReadOnly}
	}
	return out
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

func ctlEntry(e session.Entry) ctl.Entry {
	return ctl.Entry{ID: e.ID, ParentID: e.ParentID, Type: string(e.Type), TS: e.TS, Data: e.Data}
}
