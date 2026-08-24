package session

import (
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// ReplayOptions controls how a resume is condensed onto the sink.
type ReplayOptions struct {
	Thinking bool // off by default; thinking is noise on resume
}

// Replay echoes the branch's committed history onto sink: user prompts render
// above their replies (UserPrompt), assistant content streams, tool headers carry
// their result bodies, notices replay, and each turn closes with its stop reason.
// Thinking is off unless opts.Thinking requests it.
func Replay(branch []Entry, sink agent.Sink, opts ReplayOptions) {
	var cur llm.Model
	turnOpen := false
	// per-turn accumulators; reset each time a real user prompt opens a turn so
	// every TurnEnd reports only that turn's usage and final stop reason.
	var turnUsage llm.Usage
	lastStop := llm.StopUnknown

	endTurn := func() {
		if !turnOpen {
			return
		}
		sink.TurnEnd(agent.TurnResult{Stop: lastStop, Usage: turnUsage})
		turnOpen = false
	}

	// open tool calls, keyed by call ID; headers are not committed until each
	// result arrives so a header renders directly above its body, as live output does.
	pending := make(map[string]agent.ToolCall)
	for _, e := range branch {
		switch e.Type {
		case TypeSession:
			var sd SessionData
			if err := e.Decode(&sd); err == nil && sd.Model != "" {
				cur = modelFromKey(sd.Model)
			}
		case TypeModelChange:
			var md ModelData
			if err := e.Decode(&md); err == nil && md.Model != "" {
				cur = modelFromKey(md.Model)
			}
		case TypeMessage:
			var md MessageData
			if err := e.Decode(&md); err != nil {
				continue
			}
			turnUsage.Add(md.Usage)
			// the most recent non-unknown stop closes this turn
			if md.Stop != llm.StopUnknown {
				lastStop = md.Stop
			}
			m := md.Message
			switch m.Role {
			case llm.RoleUser:
				if llm.OnlyToolResults(m.Content) {
					foldResults(pending, m.Content, sink)
					continue
				}
				// system-injected context (a staged `User Ran:` result or survey text) is not a
				// typed prompt: it never opens or echoes as its own turn on replay.
				if md.Injected {
					continue
				}
				endTurn() // close the previous turn with its accumulated state first
				turnUsage = llm.Usage{}
				lastStop = llm.StopUnknown
				sink.TurnStart(agent.TurnInfo{Model: cur, Input: agent.Input{Text: userText(m)}})
				sink.UserPrompt(userText(m))
				turnOpen = true
			case llm.RoleAssistant:
				for _, b := range m.Content {
					switch blk := b.(type) {
					case llm.TextBlock:
						sink.Text(blk.Text)
						sink.EndText()
					case llm.ThinkingBlock:
						if opts.Thinking && strings.TrimSpace(blk.Text) != "" {
							sink.Thinking(blk.Text)
							sink.EndThinking()
						}
					case llm.ToolCallBlock:
						// labels on resume are a follow-up; replay falls back to the bare name.
						pending[blk.ID] = agent.ToolCall{ID: blk.ID, Name: blk.Name, Input: blk.Input}
					}
				}
			default:
				continue
			}
		case TypeNotice:
			var nd NoticeData
			if err := e.Decode(&nd); err == nil {
				sink.Notice(nd.Message, nd.Level)
			}
		default:
			// compaction / setting_change are not replayed to the UI
		}
	}
	endTurn()
}

// foldResults resolves every tool result in content against pending calls. Each
// header is committed only here, directly above its body, so replay interleaves a
// call's label and output the way live streaming does; the sink's completion hook
// commits the body through its own output-head / collapse rules to keep history bounded.
func foldResults(pending map[string]agent.ToolCall, content llm.BlockList, sink agent.Sink) {
	for _, b := range content {
		tr, ok := b.(llm.ToolResultBlock)
		if !ok {
			continue
		}
		call, ok2 := pending[tr.CallID]
		if !ok2 {
			continue
		}
		sink.ToolStart(call, call.Name)(agent.ToolResult{Content: tr.Content, Display: toolBody(tr), IsError: tr.IsError})
		delete(pending, tr.CallID)
	}
}

// toolBody joins a result's text blocks into the body streaming would have shown.
func toolBody(tr llm.ToolResultBlock) string {
	var parts []string
	for _, b := range tr.Content {
		if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// userText extracts the plain text of a prompt message.
func userText(m llm.Message) string {
	var parts []string
	for _, b := range m.Content {
		if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, " ")
}

// modelFromKey splits a provider/id key into a displayable model.
func modelFromKey(key string) llm.Model {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return llm.Model{Provider: key[:i], ID: key[i+1:]}
	}
	return llm.Model{ID: key}
}
