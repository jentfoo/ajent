package compact

import (
	"encoding/json"
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tools"
)

// Replacement markers. They are worded to explain themselves, so the summariser
// needs no instruction about what it is looking at.
const (
	dupMarker            = "[identical to an earlier tool result]"
	supersededReadMarker = "[superseded by a later read of the same file]"
	supersededEditMarker = "[superseded by a later write of the same file]"
)

// callInfo is what a tool call contributes to detection: its name and the
// canonical path it targeted (empty for calls that do not take one).
type callInfo struct {
	name string
	path string
}

// spanStubs returns what the summariser should read in place of the raw tool
// results in [lo, band): superseded reads and edits, output identical to an
// earlier result, and failed results reduced to their first line.
//
// Detection runs over the whole in-context region [lo, len(branch)) but emission
// stops at band. Narrowing detection instead would blind the best rules — a read
// superseded by a later read inside the band would go unnoticed — while narrowing
// emission is what keeps the band verbatim.
func spanStubs(branch []session.Entry, lo, band int, cwd string) []session.Stub {
	lo = max(lo, 0)
	resolve := tools.PathPolicy{Cwd: cwd}.Resolve

	calls := make(map[string]callInfo)
	reads := make(map[string][]string) // canonical path -> ordered read call ids
	writes := make(map[string][]int)   // canonical path -> branch index of each write

	for i := lo; i < len(branch); i++ {
		m, ok := messageAt(branch, i)
		if !ok || m.Role != llm.RoleAssistant {
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
					writes[p] = append(writes[p], i) // position, so a later edit is kept
				}
			}
		}
	}

	var out []session.Stub
	seen := make(map[string]string) // "name\x00path\x00text" -> first call id producing it

	for i := lo; i < len(branch); i++ {
		m, ok := messageAt(branch, i)
		if !ok || m.Role != llm.RoleUser || !llm.OnlyToolResults(m.Content) {
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
			text := resultText(tr)

			// the key includes name and path so two distinct results that happen to
			// share bytes are not collapsed into each other
			key := c.name + "\x00" + c.path + "\x00" + text
			first, dup := "", false
			if text != "" {
				first, dup = seen[key]
				if !dup {
					seen[key] = tr.CallID
				}
			}
			if i >= band {
				continue // detected for the rules above; never emitted into the band
			}

			switch {
			case tr.IsError:
				out = append(out, session.Stub{CallID: tr.CallID, Text: failedStub(c.name, tr)})
			case dup && first != tr.CallID:
				out = append(out, session.Stub{CallID: tr.CallID, Text: dupMarker})
			case c.name == "read" && superseded(reads[c.path], tr.CallID):
				out = append(out, session.Stub{CallID: tr.CallID, Text: supersededReadMarker})
			case c.name == "edit" && c.path != "" && writtenAfter(writes[c.path], i):
				out = append(out, session.Stub{CallID: tr.CallID, Text: supersededEditMarker})
			}
		}
	}

	return out
}

// messageAt decodes the message at branch[i], reporting false for entries that
// carry none and for system-role messages, which are never part of a transcript.
func messageAt(branch []session.Entry, i int) (llm.Message, bool) {
	if branch[i].Type != session.TypeMessage {
		return llm.Message{}, false
	}
	var md session.MessageData
	if err := branch[i].Decode(&md); err != nil || md.Message.Role == llm.RoleSystem {
		return llm.Message{}, false
	}
	return md.Message, true
}

// failedStub builds the one-line replacement for a failed result, keeping its
// first line so a denied reason survives.
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

// superseded reports whether callID is not the last read of its path.
func superseded(ids []string, callID string) bool {
	if len(ids) < 2 {
		return false
	}
	return ids[len(ids)-1] != callID
}

// writtenAfter reports whether any recorded write to a path occurs after branch
// position at. Indices are appended in scan order, so the last is the largest;
// only edits that precede a later wholesale rewrite get stubbed.
func writtenAfter(idx []int, at int) bool {
	if len(idx) == 0 {
		return false
	}
	return idx[len(idx)-1] > at
}
