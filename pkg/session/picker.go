package session

import (
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// RowKind tells how a branch entry reads in the rewind picker.
type RowKind int

const (
	RowUser       RowKind = iota // user prompt: first line, truncated
	RowAssistant                 // assistant text reply
	RowTool                      // tool header or result, collapsed to one label
	RowCompaction                // a context reduction, labelled by before/after tokens
)

// PickerRow is one selectable prior message for rewinding onto.
type PickerRow struct {
	ID      string // entry id to SetHead onto
	Ordinal int    // position from the root (1-based), so "continue from #12" reads naturally
	Kind    RowKind
	Label   string // condensed snippet or collapsed tool label
}

// PickerRows builds rewind rows over a branch, newest first. User and assistant
// messages show their first text line; entries that only carry tool calls or
// results collapse to one label each. Ordinals count pickable rows from the root,
// so "continue from #12" reads as the twelfth listed message.
func PickerRows(branch []Entry) []PickerRow {
	var out []PickerRow
	for _, e := range branch {
		r := rowFor(e)
		if r == nil {
			continue
		}
		r.Ordinal = len(out) + 1
		out = append(out, *r)
	}
	slices.Reverse(out) // the picker lists newest first
	return out
}

// TreeRow is one node of the context tree shown in the rewind picker.
type TreeRow struct {
	ID     string // entry id to SetHead onto when rewinding here
	Depth  int    // how many forks sit above this node (0 = on the trunk)
	Kind   RowKind
	Label  string
	Active bool   // on the current head's path; abandoned forks are not
	Guide  string // box-drawing branch prefix, e.g. "├──", "│  └──"; empty for flat rows
}

// TreeRows walks a whole transcript as a tree and emits one row per pickable
// message, oldest (root) first so branches read top-down. Newer continuations of
// a fork are appended after older ones, so the most recent work sits at the bottom
// — near where the rewind picker opens.
func TreeRows(entries []Entry, head string) []TreeRow {
	if len(entries) == 0 {
		return nil
	}
	idx := make(map[string]int, len(entries))
	children := make(map[string][]string)
	parentOf := make(map[string]string, len(entries))
	for i, e := range entries {
		idx[e.ID] = i
		if e.ParentID != "" {
			children[e.ParentID] = append(children[e.ParentID], e.ID)
			parentOf[e.ID] = e.ParentID
		}
	}

	activeSet := make(map[string]bool) // the root..head chain, marked live
	for id := head; id != ""; {
		i, ok := idx[id]
		if !ok || activeSet[id] { // unknown or already-walked (cycle guard)
			break
		}
		activeSet[id] = true
		id = entries[i].ParentID
	}

	depth := make(map[string]int, len(entries))
	var order []string
	var visit func(id string)
	visit = func(id string) {
		order = append(order, id)
		for _, c := range children[id] { // insertion order: a new branch lands at the bottom
			kids := children[id]
			if len(kids) > 1 {
				depth[c] = depth[id] + 1 // every branch of a fork goes down one level together
			} else {
				depth[c] = depth[id] // linear continuation stays flat
			}
			visit(c)
		}
	}
	// every root (usually just the session line) starts a tree; orphans too
	for _, e := range entries {
		switch e.ParentID {
		case "":
			depth[e.ID] = 0
			visit(e.ID)
		default:
			if _, ok := idx[e.ParentID]; !ok { // a child whose parent is missing stands alone
				depth[e.ID] = 0
				visit(e.ID)
			}
		}
	}

	out := make([]TreeRow, 0, len(order))
	for _, id := range order {
		e := entries[idx[id]]
		if pr := rowFor(e); pr != nil {
			out = append(out, TreeRow{
				ID:     pr.ID,
				Depth:  depth[id],
				Kind:   pr.Kind,
				Label:  pr.Label,
				Active: activeSet[id],
				Guide:  treePrefix(id, parentOf, children),
			})
		}
	}
	return out
}

// treePrefix builds the box-drawing prefix that draws this node's branch: a "│"
// continuation for each fork level above it (blank when that sibling was last), then
// its own connector when it is directly under a fork point.
func treePrefix(node string, parentOf map[string]string, children map[string][]string) string {
	var rev []string // node .. root
	for id := node; ; {
		rev = append(rev, id)
		p, ok := parentOf[id]
		if !ok || p == "" {
			break
		}
		id = p
	}
	n := len(rev)
	path := make([]string, n) // root .. node
	for i := range rev {
		path[i] = rev[n-1-i]
	}

	var cols []string
	// continuation bars for forks strictly above this node's own connector.
	for j := 0; j < n-2; j++ {
		kids := children[path[j]]
		if len(kids) > 1 { // a fork level: does the branch toward node continue past it?
			if dispLast(kids, path[j+1]) {
				cols = append(cols, "   ")
			} else {
				cols = append(cols, "│  ")
			}
		}
	}

	out := strings.Join(cols, "")
	if p, ok := parentOf[node]; ok { // node is directly under a fork -> its connector
		if kids := children[p]; len(kids) > 1 {
			if dispLast(kids, node) {
				out += "└── "
			} else {
				out += "├── "
			}
		}
	}
	return out
}

// dispLast reports whether id is the last child in display order. Children are
// listed oldest first, so a new branch (appended later) lands at the bottom and
// closes its fork with └──.
func dispLast(kids []string, id string) bool {
	for i, k := range kids { // insertion order: newest sibling is last
		if k == id {
			return i == len(kids)-1
		}
	}
	return true
}

// RewindTarget maps selecting one row onto the new branch head and editor
// pre-fill. A user message rewinds to its parent (so its text can be edited or
// re-sent); an assistant, tool-result or compaction entry stays as its own head;
// a compaction rewinds past it so context returns to just before the reduction.
func RewindTarget(entries []Entry, rowID string) (head, fill string, ok bool) {
	for i := range entries {
		e := entries[i]
		if e.ID != rowID {
			continue
		}
		switch e.Type {
		case TypeCompaction:
			return e.ParentID, "", true // parent is a valid head when it exists; caller checks ok via empty check? keep simple: return parent even if root
		case TypeMessage:
			var md MessageData
			if err := e.Decode(&md); err != nil {
				return "", "", false
			}
			switch md.Message.Role {
			case llm.RoleUser:
				if llm.OnlyToolResults(md.Message.Content) {
					return e.ID, "", true // a tool result stays as its own head
				}
				return e.ParentID, EntryMessageText(e), true // user prompt: rewind before it and pre-fill
			case llm.RoleAssistant:
				return e.ID, "", true // assistant reply keeps its own message
			default:
				return "", "", false
			}
		default:
			return "", "", false
		}
	}
	return "", "", false
}

// EntryMessageText returns the full plain-text of a user or assistant message
// entry, untruncated and newline-preserving. It is what rewinding onto that
// message pre-fills into the editor so it can be edited or re-sent.
func EntryMessageText(e Entry) string {
	if e.Type != TypeMessage {
		return ""
	}
	var md MessageData
	if err := e.Decode(&md); err != nil {
		return ""
	}
	switch md.Message.Role {
	case llm.RoleUser, llm.RoleAssistant:
	default:
		return "" // tool results and other roles are not prompt text
	}
	return messageText(md)
}

// messageText joins an entry's non-empty text blocks into one newline-delimited string.
func messageText(md MessageData) string {
	var parts []string
	for _, b := range md.Message.Content {
		if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func rowFor(e Entry) *PickerRow {
	switch e.Type {
	case TypeCompaction:
		var cd CompactionData
		if err := e.Decode(&cd); err != nil || (cd.Before == 0 && cd.After == 0) {
			return nil // unreadable or not yet measured; nothing to label
		}
		lbl := "compaction: " + strutil.FormatTokens(cd.Before) + " → " + strutil.FormatTokens(cd.After)
		if cd.Summary != "" {
			lbl += " · summarized"
		}
		return &PickerRow{ID: e.ID, Kind: RowCompaction, Label: lbl}
	case TypeMessage:
	default:
		return nil // session / notice / custom are not rewound onto
	}
	var md MessageData
	if err := e.Decode(&md); err != nil {
		return nil
	}
	m := md.Message
	switch m.Role {
	case llm.RoleUser:
		if llm.OnlyToolResults(m.Content) {
			lbl := toolResultLabel(m)
			if lbl == "" {
				return nil
			}
			return &PickerRow{ID: e.ID, Kind: RowTool, Label: lbl}
		}
		txt := strings.TrimSpace(userText(m))
		if txt == "" {
			return nil
		}
		return &PickerRow{ID: e.ID, Kind: RowUser,
			Label: "user: " + truncate(strutil.FirstLine(txt))}
	case llm.RoleAssistant:
		txt := strings.TrimSpace(userText(m))
		if txt != "" {
			return &PickerRow{ID: e.ID, Kind: RowAssistant,
				Label: "assistant: " + truncate(strutil.FirstLine(txt))}
		}
		lbl := toolCallLabel(m)
		if lbl == "" {
			return nil
		}
		return &PickerRow{ID: e.ID, Kind: RowTool, Label: lbl}
	default:
		return nil // system and other roles are not shown in the picker
	}
}

// toolResultLabel collapses a user message's tool results into one summary line.
func toolResultLabel(m llm.Message) string {
	var parts []string
	for _, b := range m.Content {
		if tr, ok := b.(llm.ToolResultBlock); ok {
			if s := strings.TrimSpace(summarize(tr)); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return truncate(strings.Join(parts, " "))
}

// toolCallLabel renders assistant tool calls as [name] args collapsed labels.
func toolCallLabel(m llm.Message) string {
	var parts []string
	for _, b := range m.Content {
		if tc, ok := b.(llm.ToolCallBlock); ok {
			lbl := "[" + tc.Name + "]"
			if s := strings.TrimSpace(strutil.FirstArgText(tc.Input)); s != "" {
				lbl += " " + truncate(strutil.FirstLine(s))
			}
			parts = append(parts, lbl)
		}
	}
	return strings.Join(parts, " ")
}
