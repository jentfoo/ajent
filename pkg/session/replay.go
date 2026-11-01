package session

import (
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// ReplayOptions controls how a resume is condensed onto the sink.
type ReplayOptions struct {
	Thinking bool // off by default, thinking is noise on resume
}

// userEcho classifies one decoded user message: which standalone shape, if
// any, it paints. Replay and EchoEntry share this rule, so full replay and a
// live append of the same message can never disagree on what it is.
type userEcho uint8

const (
	userEchoNone     userEcho = iota // not a user message: the assistant branch owns it
	userEchoPrompt                   // a typed prompt: echoes, opens a turn
	userEchoCarrier                  // tool results only: folds under headers
	userEchoInjected                 // system context: paints only when replay-marked
)

func classifyUserEcho(md MessageData) userEcho {
	if md.Message.Role != llm.RoleUser {
		return userEchoNone
	}
	switch {
	case md.Injected:
		return userEchoInjected
	case llm.OnlyToolResults(md.Message.Content):
		return userEchoCarrier
	default:
		return userEchoPrompt
	}
}

// Replay echoes the branch's committed history onto sink: user prompts render
// above their replies (UserPrompt), assistant content streams, tool headers carry
// their result bodies, notices replay, and each turn closes with its stop reason.
// Thinking is off unless opts.Thinking requests it.
func Replay(branch []Entry, sink agent.Sink, opts ReplayOptions) {
	var cur llm.Model
	var turnOpen bool
	// per-turn accumulators, reset each time a real user prompt opens a turn so
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

	// open tool calls keyed by call ID, headers committed only once each
	// result arrives so a header renders directly above its body as live output does.
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
				endTurn() // the lost line may have been the prompt bounding this turn
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
				text := userText(m)
				switch classifyUserEcho(md) {
				case userEchoCarrier:
					foldResults(pending, m.Content, sink)
					continue
				case userEchoInjected:
					// injected context opens no turn; replay-marked ones draw as their own block
					if md.Replayed {
						agent.InjectBlock(e.ID, text, sink)
					}
					continue
				}
				endTurn() // close the previous turn with its accumulated state first
				turnUsage = llm.Usage{}
				lastStop = llm.StopUnknown
				sink.TurnStart(agent.TurnInfo{Model: cur, Input: agent.Input{Text: text}})
				sink.UserPrompt(text)
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
						// labels on resume are a follow-up, so replay falls back to the bare name
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
// call's label and output the way live streaming does. The sink's completion hook
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
		sink.ToolStart(call, call.Name, false)(agent.ToolResult{
			Content: tr.Content, Display: joinText(tr.Content, "\n"), IsError: tr.IsError})
		delete(pending, tr.CallID)
	}
}

// EchoEntry paints one committed entry's standalone shape onto sink: exactly
// the shapes Replay draws outside its turn machinery, so a live append and a
// full replay of the same entry render identically. A typed prompt echoes, a
// replay-marked injected block draws as its own block and a notice replays;
// tool carriers, unmarked injected context and everything the stream owns
// (assistant content, tool calls) paint nothing here.
func EchoEntry(e Entry, sink agent.Sink) {
	switch e.Type {
	case TypeMessage:
		var md MessageData
		if e.Decode(&md) != nil {
			return
		}
		switch classifyUserEcho(md) {
		case userEchoPrompt:
			sink.UserPrompt(userText(md.Message))
		case userEchoInjected:
			if md.Replayed {
				agent.InjectBlock(e.ID, userText(md.Message), sink)
			}
		}
	case TypeNotice:
		var nd NoticeData
		if e.Decode(&nd) == nil {
			sink.Notice(nd.Message, nd.Level)
		}
	}
}

// userText extracts the plain text of a prompt message, with one image
// placeholder appended per image block so a picture submitted with the prompt
// is never silently invisible on resume.
func userText(m llm.Message) string {
	return joinText(m.Content, " ")
}

// joinText renders content as plain text: non-blank text blocks joined by sep,
// one image placeholder per image block where it sat. Replay never redraws
// images, so each is named in place.
func joinText(content llm.BlockList, sep string) string {
	var sb strings.Builder
	for _, b := range content {
		switch blk := b.(type) {
		case llm.TextBlock:
			if strings.TrimSpace(blk.Text) != "" {
				if sb.Len() > 0 {
					sb.WriteString(sep)
				}
				sb.WriteString(blk.Text)
			}
		case llm.ImageBlock:
			if sb.Len() > 0 {
				sb.WriteString(sep)
			}
			sb.WriteString(llm.ImagePlaceholder(blk.Data, ""))
		}
	}
	return sb.String()
}

// modelFromKey splits a provider/id key into a displayable model.
func modelFromKey(key string) llm.Model {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return llm.Model{Provider: key[:i], ID: key[i+1:]}
	}
	return llm.Model{ID: key}
}
