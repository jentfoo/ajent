package session

import (
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
)

// Stub replaces one recorded tool result when context is rebuilt: Text swaps its
// content outright, Limit elides it to that many bytes. Exactly one of them is set.
type Stub struct {
	CallID string `json:"callId"`
	Text   string `json:"text,omitempty"`
	Limit  int    `json:"limit,omitzero"`
}

// Reduce is the structural reduction plan a compaction recorded. It replays on
// every rebuild so a resumed session sees exactly what the model saw.
type Reduce struct {
	Stubs         []Stub   `json:"stubs,omitempty"` // tool results replaced or elided by call id
	Drop          []string `json:"drop,omitempty"`  // message entry ids removed outright
	StripThinking bool     `json:"stripThinking,omitempty"`
	Stats         Stats    `json:"stats,omitzero"`
}

// Stats counts what each stage did, for the compaction notice.
type Stats struct {
	Failed     int `json:"failed,omitzero"`
	Superseded int `json:"superseded,omitzero"`
	Truncated  int `json:"truncated,omitzero"`
	Aborted    int `json:"aborted,omitzero"`
	Summarized int `json:"summarized,omitzero"`
}

// ContextMessages returns the messages branch contributes to the next request,
// applying cd's cut point and structural reductions. Warnings name entries it
// could not use.
func ContextMessages(branch []Entry, cd CompactionData) ([]llm.Message, []string) {
	var msgs []llm.Message
	var warns []string

	keep := cutIndex(branch, cd)
	if keep < 0 {
		warns = append(warns, "compaction first-kept entry not found")
		return nil, warns // a cut that cannot be located keeps nothing safe
	}

	var dropped map[string]struct{}
	var stubs map[string]Stub
	if r := cd.Reduce; r != nil {
		dropped = bulk.SliceToSet(r.Drop)
		stubs = make(map[string]Stub, len(r.Stubs))
		for _, s := range r.Stubs {
			stubs[s.CallID] = s
		}
	}
	stripThinking := cd.Reduce != nil && cd.Reduce.StripThinking

	if keep > 0 && cd.Summary != "" {
		msgs = append(msgs, llm.Message{
			Role:    llm.RoleUser,
			Content: llm.BlockList{llm.TextBlock{Text: summaryFraming(cd.Summary)}},
		})
	}

	for i := range branch {
		if keep > 0 && i < keep {
			continue // summarized away by the cut
		}
		e := branch[i]
		if e.Type != TypeMessage {
			continue
		}
		if _, ok := dropped[e.ID]; ok {
			continue
		}
		var md MessageData
		if err := e.Decode(&md); err != nil {
			warns = append(warns, "invalid message entry: "+err.Error())
			continue
		}
		reduced := applyReduce(md.Message, stripThinking, stubs)
		// stripping thinking can leave an assistant message with no content at all (it
		// held only thinking blocks); providers reject empty assistant messages. Drop such
		// entries only when we actually stripped — a pre-existing empty assistant is left
		// as recorded so resume keeps exactly the context it had.
		if stripThinking && md.Message.Role == llm.RoleAssistant && len(reduced.Content) == 0 {
			continue
		}
		msgs = append(msgs, reduced)
	}

	return msgs, warns
}

// cutIndex returns the branch index where cd's kept tail starts, or -1 when a
// set FirstKeptEntryID is missing from the branch.
func cutIndex(branch []Entry, cd CompactionData) int {
	if cd.FirstKeptEntryID != "" {
		for i := range branch {
			if branch[i].ID == cd.FirstKeptEntryID {
				return i
			}
		}
		return -1
	}
	if cd.Summary == "" {
		return 0 // reductions only, no truncation
	}
	// summary-only: the compaction entry itself is the boundary
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type == TypeCompaction {
			return i + 1
		}
	}
	return 0
}

// applyReduce stubs and elides tool results and strips thinking per the plan,
// returning m unchanged when nothing applies so a caller can reuse it.
func applyReduce(m llm.Message, stripThinking bool, stubs map[string]Stub) llm.Message {
	var changed bool
	out := make([]llm.Block, 0, len(m.Content))
	for _, b := range m.Content {
		switch blk := b.(type) {
		case llm.ToolResultBlock:
			if s, ok := stubs[blk.CallID]; ok {
				changed = true
				out = append(out, stubResult(blk, s))
				continue
			}
		case llm.ThinkingBlock:
			if stripThinking {
				changed = true
				continue
			}
		}
		out = append(out, b)
	}
	if !changed {
		return m // nothing was touched; reuse the caller's message
	}
	return llm.Message{Role: m.Role, Content: out}
}

// stubResult rewrites a tool result to its stubbed or elided form.
func stubResult(tr llm.ToolResultBlock, s Stub) llm.ToolResultBlock {
	if s.Text != "" {
		tr.Content = llm.BlockList{llm.TextBlock{Text: s.Text}}
		return tr
	}
	var text string
	for _, b := range tr.Content {
		if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			text = tb.Text
			break
		}
	}
	out, _ := tools.Elide(text, tools.Limit{Bytes: s.Limit})
	tr.Content = llm.BlockList{llm.TextBlock{Text: out}}
	return tr
}

// summaryFraming wraps a compaction summary with its provenance so the model
// knows what it lost. Kept as a user message because providers reject a leading
// system message that is not the top-level system field.
func summaryFraming(s string) string {
	if s == "" {
		return ""
	}
	const head = "The conversation history before this point was compacted into the following summary:\n\n"
	return head + "<summary>\n" + s + "\n</summary>"
}
