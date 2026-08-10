package session

import (
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
)

// Tip is one selectable continuation point in a transcript: the tip of some
// chain, either the active head or an abandoned fork. The resume picker lists
// every tip so no branch becomes unreachable after switching away.
type Tip struct {
	ID    string // entry id to Branch from and SetHead onto
	First string // first user message on that branch, for display; empty if none
}

// Tips returns every chain tip in file order — entries whose id is not another
// entry's parent. The persisted head and each fork appear exactly once.
func Tips(entries []Entry) []Tip {
	if len(entries) == 0 {
		return nil
	}
	parented := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.ParentID != "" {
			parented[e.ParentID] = true
		}
	}
	var out []Tip
	for _, e := range entries {
		if e.ID == "" || parented[e.ID] {
			continue
		}
		out = append(out, Tip{ID: e.ID, First: firstUserOn(Branch(entries, e.ID))})
	}
	return out
}

// firstUserOn returns the first user text on a branch, truncated for display.
func firstUserOn(branch []Entry) string {
	for _, e := range branch {
		if e.Type != TypeMessage {
			continue
		}
		var md MessageData
		if err := e.Decode(&md); err != nil || md.Message.Role != llm.RoleUser {
			continue
		}
		for _, b := range md.Message.Content {
			if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
				return truncate(strings.TrimSpace(tb.Text))
			}
		}
	}
	return ""
}
