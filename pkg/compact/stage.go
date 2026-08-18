package compact

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tools"
)

// callInfo is what a tool call contributes to stage detection: its name and the
// canonical path it targeted (empty for calls that do not take one).
type callInfo struct {
	name string
	path string
}

// structural runs stage 1 over the branch: failed/denied results older than two
// turns, superseded reads and edits, and aborted assistant messages. It returns
// stubs by tool-result call id plus entry ids to drop outright.
func structural(branch []session.Entry, cwd string) (stubs []session.Stub, drops []string, stats session.Stats) {
	resolve := tools.PathPolicy{Cwd: cwd}.Resolve

	calls := make(map[string]callInfo)
	reads := make(map[string][]string) // canonical path -> ordered read call ids
	writes := make(map[string][]int)   // canonical path -> branch index of each `write`
	turns, totalTurns := turnCounts(branch)

	for i := range branch {
		e := &branch[i]
		if e.Type != session.TypeMessage || turns[i] < 0 {
			continue
		}
		var md session.MessageData
		if err := e.Decode(&md); err != nil {
			continue
		}
		m := md.Message
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			tc, ok := b.(llm.ToolCallBlock)
			if !ok {
				continue
			}
			p := canonicalPath(resolve, tc.Input)
			calls[tc.ID] = callInfo{name: tc.Name, path: p}
			switch tc.Name {
			case "read":
				if p != "" {
					reads[p] = append(reads[p], tc.ID)
				}
			case "write":
				if p != "" {
					writes[p] = append(writes[p], i) // position, so an edit after the write is kept
				}
			}
		}
	}

	for i := range branch {
		e := &branch[i]
		if e.Type != session.TypeMessage || turns[i] < 0 {
			continue
		}
		var md session.MessageData
		if err := e.Decode(&md); err != nil {
			continue
		}
		m := md.Message

		// aborted assistant messages carry no tool calls and no non-blank text.
		if m.Role == llm.RoleAssistant && !hasToolCall(m.Content) && blankText(m.Content) {
			drops = append(drops, e.ID)
			stats.Aborted++
			continue
		}
		if m.Role != llm.RoleUser || !llm.OnlyToolResults(m.Content) {
			continue
		}

		for _, b := range m.Content {
			tr, ok := b.(llm.ToolResultBlock)
			if !ok {
				continue
			}
			c, has := calls[tr.CallID]
			if !has {
				continue
			}
			switch {
			case tr.IsError && turns[i] <= totalTurns-2: // older than the last two turns
				stubs = append(stubs, session.Stub{CallID: tr.CallID, Text: failedStub(c.name, tr)})
				stats.Failed++
			case c.name == "read" && superseded(reads[c.path], tr.CallID):
				stubs = append(stubs, session.Stub{
					CallID: tr.CallID,
					Text:   "[read superseded by a later read of the same file]",
				})
				stats.Superseded++
			case c.name == "edit" && c.path != "" && writtenAfter(writes[c.path], i):
				stubs = append(stubs, session.Stub{
					CallID: tr.CallID,
					Text:   "[edits superseded by a later write of the same file]",
				})
				stats.Superseded++
			}
		}
	}

	return stubs, drops, stats
}

// truncate runs stage 2 over the branch: age-tiered elision and duplicate-output
// collapse for successful tool results not already handled by structural. It
// appends to r.Stubs via a fresh slice so callers can measure per-stage.
func truncate(branch []session.Entry, cwd string, r *session.Reduce) []session.Stub {
	already := bulk.SliceToSetBy(func(s session.Stub) string { return s.CallID }, r.Stubs)
	turns, totalTurns := turnCounts(branch)
	resolve := tools.PathPolicy{Cwd: cwd}.Resolve
	calls := make(map[string]callInfo) // call id -> name+path for duplicate keys
	var out []session.Stub
	seen := make(map[string]string) // "name\x00path\x00text" -> first call id that produced it

	for i := range branch {
		e := &branch[i]
		if e.Type != session.TypeMessage || turns[i] < 0 {
			continue
		}
		var md session.MessageData
		if err := e.Decode(&md); err != nil {
			continue
		}
		m := md.Message
		if m.Role == llm.RoleAssistant { // index the producing call for its name/path
			for _, b := range m.Content {
				if tc, ok := b.(llm.ToolCallBlock); ok {
					calls[tc.ID] = callInfo{name: tc.Name, path: canonicalPath(resolve, tc.Input)}
				}
			}
			continue
		}
		if m.Role != llm.RoleUser || !llm.OnlyToolResults(m.Content) {
			continue
		}
		for _, b := range m.Content {
			tr, ok := b.(llm.ToolResultBlock)
			if !ok || tr.IsError {
				continue // failures are structural (stage 1), not size problems
			}
			if _, done := already[tr.CallID]; done {
				continue
			}
			text := resultText(tr)
			if text == "" {
				continue
			}
			// duplicate identical output from the same target: keep the first, stub the
			// rest. The key includes name and path so two distinct results (e.g. reading
			// different files that happen to share bytes) are not collapsed.
			c := calls[tr.CallID]
			key := c.name + "\x00" + c.path + "\x00" + text
			if first, dup := seen[key]; dup && first != tr.CallID {
				out = append(out, session.Stub{CallID: tr.CallID, Text: "[duplicate of an earlier tool result]"})
				continue
			}
			seen[key] = tr.CallID

			budget := tierBudget(turns[i], totalTurns)
			if budget <= 0 || len(text) <= budget {
				continue
			}
			out = append(out, session.Stub{CallID: tr.CallID, Limit: budget})
		}
	}

	return out
}

// tierBudget returns the byte cap for a successful result in turn t of totalTurns,
// or 0 when it should be left alone (the current turn is live working set).
// Older turns shrink with age; the oldest collapse nearly to nothing.
func tierBudget(t, total int) int {
	dist := total - t // how many full turns sit after this one
	switch {
	case dist <= 0:
		return 0 // current turn: never truncate the active context
	case dist == 1:
		return 8 << 10 // recent: keep 8 kB
	case dist <= 3:
		return 1024 // older: 1 kB
	default:
		return 512 // oldest: collapse to a shape line
	}
}

// failedStub builds the one-line replacement for an old failed result, keeping
// its first line so a denied reason survives.
func failedStub(name string, tr llm.ToolResultBlock) string {
	line := strutil.FirstLine(strings.TrimSpace(resultText(tr)))
	if line == "" {
		return "[tool " + name + " failed: output dropped]"
	}
	return "[tool " + name + " failed: " + strutil.Clip(line, 80) + "]"
}

// canonicalPath resolves the path argument of a tool call input to one key.
func canonicalPath(resolve func(string) (string, error), input json.RawMessage) string {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &p); err != nil || strings.TrimSpace(p.Path) == "" {
		return ""
	}
	r, err := resolve(p.Path)
	if err != nil {
		return p.Path // bare fallback keeps superseded detection on a stable key
	}
	return r
}

// resultText returns the first non-blank text block of a tool result.
func resultText(tr llm.ToolResultBlock) string {
	for _, b := range tr.Content {
		if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			return tb.Text
		}
	}
	return ""
}

// turnCounts assigns each branch entry the turn number of its message (a real user
// prompt opens a new turn; tool results stay in their own), and returns that plus
// the total number of turns. Non-message entries get -1.
func turnCounts(branch []session.Entry) (turns []int, total int) {
	turns = make([]int, len(branch))
	for i := range branch {
		turns[i] = -1
		if branch[i].Type != session.TypeMessage {
			continue
		}
		var md session.MessageData
		if err := branch[i].Decode(&md); err != nil || md.Message.Role == llm.RoleSystem {
			continue
		}
		m := md.Message
		switch m.Role {
		case llm.RoleUser:
			if !llm.OnlyToolResults(m.Content) {
				total++ // a real user prompt opens the next turn
			}
		}
		turns[i] = total
	}
	return turns, total
}

// superseded reports whether callID is not the last read of its path.
func superseded(ids []string, callID string) bool {
	if len(ids) < 2 {
		return false
	}
	return ids[len(ids)-1] != callID
}

// writtenAfter reports whether any recorded write to a path occurs after branch
// position at. Indices are appended in scan order, so the last is the largest; only
// edits that precede a later wholesale rewrite get stubbed.
func writtenAfter(idx []int, at int) bool {
	if len(idx) == 0 {
		return false
	}
	return idx[len(idx)-1] > at
}

// hasToolCall reports whether a message carries any tool-call block.
func hasToolCall(content llm.BlockList) bool {
	return slices.ContainsFunc(content, func(b llm.Block) bool {
		_, ok := b.(llm.ToolCallBlock)
		return ok
	})
}

// blankText reports whether every text block of a message is empty or whitespace.
func blankText(content llm.BlockList) bool {
	var saw bool
	for _, b := range content {
		if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			saw = true
		}
	}
	return !saw
}
