package session

import (
	"slices"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/llm"
)

// CopyTexts returns the clipboard payload for each entry id in ids, in order:
// a user prompt's full text, an assistant turn as text plus verbatim
// call/result JSON, a tool row as its call and result JSON, a compaction as
// its summary text. Unrenderable ids yield "". The payload is the structured
// content from the transcript, never the picker's display label, so copies
// stay truncation-independent.
func CopyTexts(entries []Entry, ids []string) []string {
	ti := newTreeIndex(entries)
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = copyEntryText(ti, id)
	}
	return out
}

// copyEntryText renders one entry's clipboard payload, "" when unrenderable.
func copyEntryText(ti treeIndex, id string) string {
	e, ok := ti.entry(id)
	if !ok {
		return ""
	}
	if e.Type == TypeCompaction {
		var cd CompactionData
		if err := e.Decode(&cd); err != nil {
			return ""
		}
		return cd.Summary
	}
	if e.Type != TypeMessage {
		return ""
	}
	var md MessageData
	if err := e.Decode(&md); err != nil {
		return ""
	}
	m := md.Message
	switch {
	case m.Role == llm.RoleAssistant:
		return llm.CopyAssistant(turnMessages(ti, e, m), 0)
	case m.Role == llm.RoleUser && llm.OnlyToolResults(m.Content):
		return resultText(ti, e, m)
	case m.Role == llm.RoleUser:
		return EntryMessageText(e)
	default:
		return ""
	}
}

// turnMessages collects an assistant message plus the result messages that
// answer it, walking the entry's own chain until the next assistant message
// or until every call has its result, whichever comes first.
func turnMessages(ti treeIndex, e Entry, m llm.Message) []llm.Message {
	msgs := []llm.Message{m}
	if !messageHasCalls(m) {
		return msgs
	}
	for id := ti.firstChild(e.ID); id != ""; id = ti.firstChild(id) {
		child, ok := ti.entry(id)
		if !ok || child.Type != TypeMessage {
			break
		}
		var md MessageData
		if err := child.Decode(&md); err != nil || md.Message.Role == llm.RoleAssistant {
			break
		}
		msgs = append(msgs, md.Message)
		if callsAnswered(m, msgs[1:]) {
			break
		}
	}
	return msgs
}

// resultText renders a tool-results message together with the calls it
// answers, walking parents until every result call id has its call.
func resultText(ti treeIndex, e Entry, m llm.Message) string {
	need := resultCallIDs(m)
	chain := make([]llm.Message, 0, 1)
	for id := ti.parentOf[e.ID]; id != "" && len(need) > 0; id = ti.parentOf[id] {
		pe, ok := ti.entry(id)
		if !ok || pe.Type != TypeMessage {
			break
		}
		var md MessageData
		if err := pe.Decode(&md); err != nil {
			break
		}
		chain = append(chain, md.Message)
		need = bulk.SliceFilter(func(callID string) bool {
			return !messageHasCall(md.Message, callID)
		}, need)
	}
	slices.Reverse(chain)
	return llm.CopyResults(append(chain, m), len(chain))
}

// messageHasCalls reports whether m carries any tool call.
func messageHasCalls(m llm.Message) bool {
	return slices.ContainsFunc(m.Content, func(b llm.Block) bool {
		_, ok := b.(llm.ToolCallBlock)
		return ok
	})
}

// messageHasCall reports whether m carries the tool call named callID.
func messageHasCall(m llm.Message, callID string) bool {
	return slices.ContainsFunc(m.Content, func(b llm.Block) bool {
		call, ok := b.(llm.ToolCallBlock)
		return ok && call.ID == callID
	})
}

// callsAnswered reports whether every call in the assistant message has a
// result among the collected messages.
func callsAnswered(call llm.Message, rest []llm.Message) bool {
	for _, b := range call.Content {
		tc, ok := b.(llm.ToolCallBlock)
		if !ok {
			continue
		}
		answered := slices.ContainsFunc(rest, func(m llm.Message) bool {
			return messageHasCall(m, tc.ID)
		})
		if !answered {
			return false
		}
	}
	return true
}

// resultCallIDs lists the call ids a results message answers.
func resultCallIDs(m llm.Message) []string {
	ids := make([]string, 0, len(m.Content))
	for _, b := range m.Content {
		if r, ok := b.(llm.ToolResultBlock); ok && r.CallID != "" {
			ids = append(ids, r.CallID)
		}
	}
	return ids
}
