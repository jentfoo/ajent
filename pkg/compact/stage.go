package compact

import (
	"encoding/json"
	"strings"

	"github.com/go-analyze/bulk"

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

// writeCall joins a write's call id to the branch index of its message, so the
// result-status filter keeps each write's ordering key.
type writeCall struct {
	at     int
	callID string
}

// spanStubs returns what the summariser should read in place of the raw tool
// results in [lo, band): reads and edits superseded by a successful later
// access to the same file, output identical to an earlier result, and failed
// results reduced to their first line.
//
// Detection runs over the whole in-context region [lo, len(branch)) but emission
// stops at band. Narrowing detection too would hide a real win, such as a read
// superseded by a later read inside the band that otherwise goes unnoticed,
// while narrowing emission alone is what keeps the band verbatim.
func spanStubs(branch []session.Entry, lo, band int, cwd string) []session.Stub {
	lo = max(lo, 0)
	resolve := tools.PathPolicy{Cwd: cwd}.Resolve

	calls := make(map[string]callInfo)
	succeeded := make(map[string]struct{}) // call ids whose result reported success
	reads := make(map[string][]string)     // canonical path -> ordered read call ids
	writes := make(map[string][]writeCall) // canonical path -> ordered write calls with branch positions

	for i := lo; i < len(branch); i++ {
		m, ok := messageAt(branch, i)
		if !ok {
			continue
		}
		switch m.Role {
		case llm.RoleAssistant:
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
						writes[p] = append(writes[p], writeCall{at: i, callID: tc.ID})
					}
				}
			}
		case llm.RoleUser:
			for _, b := range m.Content {
				tr, ok := b.(llm.ToolResultBlock)
				if !ok || tr.IsError {
					continue
				}
				succeeded[tr.CallID] = struct{}{}
			}
		}
	}

	// supersession is a claim about file state, so only a successful result makes
	// the claim true: a failed or denied access counts as absent.
	didSucceed := func(id string) bool { _, ok := succeeded[id]; return ok }
	for p, ids := range reads {
		reads[p] = bulk.SliceFilterInPlace(didSucceed, ids)
	}
	for p, wcs := range writes {
		writes[p] = bulk.SliceFilterInPlace(func(wc writeCall) bool { return didSucceed(wc.callID) }, wcs)
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
			text := llm.FirstBlockText(tr.Content)

			// the key includes name and path so two distinct results that happen to
			// share bytes are not collapsed into each other
			key := c.name + "\x00" + c.path + "\x00" + text
			var first string
			var dup bool
			if text != "" {
				first, dup = seen[key]
				if !dup {
					seen[key] = tr.CallID
				}
			}
			if i >= band {
				continue // detected for the rules above, never emitted into the band
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
	line := strutil.FirstLine(strings.TrimSpace(llm.FirstBlockText(tr.Content)))
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

// superseded reports whether callID is not the newest successful read of its
// path; registration already dropped reads that failed or were denied.
func superseded(ids []string, callID string) bool {
	if len(ids) < 2 {
		return false
	}
	return ids[len(ids)-1] != callID
}

// writtenAfter reports whether the newest write registered for a path landed
// after branch position at; only successful writes are registered, appended in
// scan order, so the last entry is the newest.
func writtenAfter(wcs []writeCall, at int) bool {
	if len(wcs) == 0 {
		return false
	}
	return wcs[len(wcs)-1].at > at
}
