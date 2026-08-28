package session

import (
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTreeRows covers fork rendering in the tree view.
func TestTreeRows(t *testing.T) {
	t.Parallel()

	// an un-forked chat stays one flat column: every message at depth 0 with no branch connectors.
	t.Run("flat_linear", func(t *testing.T) {
		entries := []Entry{
			sessionOnly("root"),
			pickMsg("u1", "root", llm.Text(llm.RoleUser, "hihi hi again")),
			pickAssistText("a2", "u1", "Hi!"),
			pickMsg("u3", "a2", llm.Text(llm.RoleUser, "idk how do you feel?")),
			pickAssistText("a4", "u3", "I don't have feelings per se"),
		}

		tree := TreeRows(entries, "a4")
		require.Len(t, tree, 4) // session entry skipped

		// pre-order from the root: oldest first along a single chain.
		assert.Equal(t, []string{"u1", "a2", "u3", "a4"}, idsOf(tree))
		for _, r := range tree {
			assert.Equalf(t, 0, r.Depth, "linear chain must stay flat (id=%s)", r.ID)
			assert.Emptyf(t, r.Guide, "no fork means no branch connector (id=%s)", r.ID)
		}
		// the whole active path is live.
		for _, r := range tree {
			assert.Truef(t, r.Active, "every node of a linear chat is on the head's path (id=%s)", r.ID)
		}
	})

	// every pickable kind shows up in pre-order with a collapsed label; the session
	// entry never does.
	t.Run("mixed_entry_kinds", func(t *testing.T) {
		entries := []Entry{
			sessionOnly("root"),
			pickMsg("u1", "root", llm.Text(llm.RoleUser, "first question")),
			pickAssistText("a1", "u1", "an answer"),
			pickToolCall("t1", "a1"), // assistant turn ending in a tool call
			pickToolResultMsg("r1", "t1"),
			pickCompaction("c1", "r1"),
		}

		tree := TreeRows(entries, "c1")
		require.Len(t, tree, 5) // session entry is skipped

		assert.Equal(t, []string{"u1", "a1", "t1", "r1", "c1"}, idsOf(tree))
		assert.Equal(t, RowUser, tree[0].Kind)
		assert.Equal(t, RowAssistant, tree[1].Kind)
		assert.Equal(t, RowTool, tree[2].Kind)
		assert.Contains(t, tree[2].Label, "[bash]") // collapsed call shows [name] args
		assert.Equal(t, RowTool, tree[3].Kind)
		assert.Contains(t, tree[3].Label, "line one") // result collapses to its first line
		assert.Equal(t, RowCompaction, tree[4].Kind)
	})

	// rewinding + resubmitting produces two branches at the SAME level: both children
	// of the fork point move down one together, drawn with connectors. The most recent branch lists first.
	t.Run("shows_fork", func(t *testing.T) {
		entries := []Entry{
			sessionOnly("root"),
			pickMsg("u1", "root", llm.Text(llm.RoleUser, "first question")),
		}
		// u1 forks into a1 (old) and u2->a2 (newer).
		forked := append([]Entry(nil), entries...)
		forked = append(forked,
			pickAssistText("a1", "u1", "an answer"), // old, abandoned
			pickMsg("u2", "u1", llm.Text(llm.RoleUser, "unrelated fork")),
			pickAssistText("a2", "u2", "the other reply"))

		// head is a2; u1 and u2 are active, a1 is an abandoned fork.
		tree := TreeRows(forked, "a2")
		require.Len(t, tree, 4) // u1 + (a1 | u2,a2)

		// pre-order in insertion order: older sibling first, newer branch last (bottom).
		assert.Equal(t, []string{"u1", "a1", "u2", "a2"}, idsOf(tree))

		depth := map[string]int{}
		guide := map[string]string{}
		active := map[string]bool{}
		for _, r := range tree {
			depth[r.ID], guide[r.ID], active[r.ID] = r.Depth, r.Guide, r.Active
		}
		// both siblings of the fork sit at depth 1 together; the shared root stays flat.
		assert.Equal(t, 0, depth["u1"])
		assert.Equal(t, 1, depth["a1"], "older branch indented one level")
		assert.Equal(t, 1, depth["u2"], "newer branch at the SAME level as its sibling")
		assert.Equal(t, 1, depth["a2"])

		// drawn with connectors: older child listed first (├──), newer closes the fork (└──).
		assert.Empty(t, guide["u1"])
		assert.Equal(t, "├── ", guide["a1"], "older sibling is the first listed branch")
		assert.Equal(t, "└── ", guide["u2"], "newer sibling closes the fork at the bottom")
		assert.Equal(t, "    ", guide["a2"], "continuation blank because its parent u2 is last (newest at bottom)")

		// u2 is the last/newest sibling -> └──; a1 is not live.
		assert.True(t, active["u1"] && active["u2"] && active["a2"])
		assert.False(t, active["a1"])
	})

	// every guide cell is four columns wide, so a branch's continuation lines up under the text of its own connector.
	t.Run("guide_alignment", func(t *testing.T) {
		entries := []Entry{
			sessionOnly("root"),
			pickMsg("u1", "root", llm.Text(llm.RoleUser, "first question")),
			pickAssistText("a1", "u1", "an answer"), // older branch, kept growing
			pickMsg("u2", "a1", llm.Text(llm.RoleUser, "follow up on the old branch")),
			pickMsg("u3", "u1", llm.Text(llm.RoleUser, "newer branch")),
		}

		guide := map[string]string{}
		for _, r := range TreeRows(entries, "u3") {
			guide[r.ID] = r.Guide
		}
		assert.Equal(t, "├── ", guide["a1"])
		assert.Equal(t, "└── ", guide["u3"])
		// u2 continues under a1, which is not the last child: a bar, four wide.
		assert.Equal(t, "│   ", guide["u2"])
		for id, g := range guide {
			assert.Zerof(t, len([]rune(g))%4, "guide cells are four columns wide (id=%s, guide=%q)", id, g)
		}
	})
}

func idsOf(rows []TreeRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// TestTreeRowLabelsAndKinds pins which entry kinds become picker rows and how each
// label collapses; rowFor's branches are reachable only through TreeRows.
func TestTreeRowLabelsAndKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   Entry
		kind RowKind
		sub  string // substring expected in Label; empty means no row
	}{
		{"user_text", pickMsg("a", "", llm.Text(llm.RoleUser, "hello world")), RowUser, "user: hello"},
		{"assistant_text", pickAssistText("b", "", "the fix is here"), RowAssistant, "assistant: the fix"},
		{"tool_call_only", pickToolCall("c", ""), RowTool, "[bash] read main.go"},
		{"tool_result_only", pickToolResultMsg("d", ""), RowTool, "line one"},
		// a staged `!` result is not a typed prompt: rewinding onto it would pre-fill
		// the editor with its whole output rather than a message
		{"injected_user_not_a_row", pickInjected("e", "", "User Ran: git status\n\nOutput:\nclean"), 0, ""},
		{"compaction_summarized", pickCompaction("f", ""), RowCompaction, "· summarized"},
		{"compaction_unmeasured", Entry{ID: "g", Type: TypeCompaction,
			Data: mustJSON(CompactionData{Summary: "s"})}, 0, ""}, // nothing to label yet
		{"session_skipped", sessionOnly("s"), 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := TreeRows([]Entry{tc.in}, "")
			if tc.sub == "" {
				assert.Empty(t, rows)
				return
			}
			require.Len(t, rows, 1)
			assert.Equal(t, tc.kind, rows[0].Kind)
			assert.Contains(t, rows[0].Label, tc.sub)
		})
	}
}

// helpers ---------------------------------------------------------

func sessionOnly(id string) Entry {
	return Entry{ID: id, Type: TypeSession}
}

func pickMsg(id, parent string, m llm.Message) Entry {
	return Entry{ID: id, ParentID: parent, Type: TypeMessage,
		Data: mustJSON(MessageData{Message: m})}
}

func pickAssistText(id, parent, text string) Entry {
	return pickMsg(id, parent, llm.Text(llm.RoleAssistant, text))
}

// pickInjected is a system-injected user message: staged `!` output or an @ read.
func pickInjected(id, parent, text string) Entry {
	return Entry{ID: id, ParentID: parent, Type: TypeMessage,
		Data: mustJSON(MessageData{Message: llm.Text(llm.RoleUser, text), Injected: true, Replayed: true})}
}

// pickToolCall is an assistant message that only carries a bash tool call.
func pickToolCall(id, parent string) Entry {
	m := llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
		llm.ToolCallBlock{ID: "c1", Name: "bash",
			Input: json.RawMessage(`{"command":"read main.go"}`)},
	}}
	return pickMsg(id, parent, m)
}

// pickToolResultMsg is a user message holding only tool results.
func pickToolResultMsg(id, parent string) Entry {
	m := llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
		llm.ToolResultBlock{CallID: "c1",
			Content: llm.BlockList{llm.TextBlock{Text: "line one\nline two"}}},
	}}
	return pickMsg(id, parent, m)
}

// TestEntryMessageText verifies the untruncated, newline-preserving prompt text
// returned for pre-filling the editor on rewind.
func TestEntryMessageText(t *testing.T) {
	t.Parallel()

	m := llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
		llm.TextBlock{Text: "line one"},
		llm.TextBlock{Text: "line two\nwith more"},
	}}
	e := pickMsg("u1", "", m)
	assert.Equal(t, "line one\nline two\nwith more", EntryMessageText(e))

	// tool-only and non-message entries carry no prompt text.
	toolOnly := llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
		llm.ToolResultBlock{CallID: "c1"},
	}}
	assert.Empty(t, EntryMessageText(pickMsg("r", "", toolOnly)))
	assert.Empty(t, EntryMessageText(sessionOnly("s")))
}

// pickCompaction is a compaction entry with measured before/after tokens.
func pickCompaction(id, parent string) Entry {
	return Entry{ID: id, ParentID: parent, Type: TypeCompaction,
		Data: mustJSON(CompactionData{Before: 142000, After: 61000, Summary: "s"})}
}

func TestRewindTarget(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		sessionOnly("root"),
		pickMsg("u1", "root", llm.Text(llm.RoleUser, "hello world")),
		pickAssistText("a1", "u1", "hi there"),
		pickToolCall("t1", "a1"),
		pickToolResultMsg("r1", "t1"),
		pickCompaction("c1", "r1"),
		// staged `!` output precedes a later prompt on the same chain
		pickInjected("i1", "c1", "User Ran: git status\n\nOutput:\nclean"),
		pickMsg("u2", "i1", llm.Text(llm.RoleUser, "what changed?")),
	}

	cases := []struct {
		name     string
		row      string
		wantHead string
		wantFill string
		wantOK   bool
	}{
		{"user_prompt_rewinds_before", "u1", "root", "hello world", true},
		{"assistant_keeps_own_message", "a1", "a1", "", true},
		{"tool_call_keeps_own_message", "t1", "t1", "", true},
		{"tool_result_keeps_own_message", "r1", "r1", "", true},
		{"compaction_rewinds_before", "c1", "r1", "", true},
		// the staged result stays in context: it is the prompt's parent, so it
		// remains on the rewound branch as the new head
		{"prompt_after_staged", "u2", "i1", "what changed?", true},
		{"unknown_row_not_ok", "nope", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, fill, ok := RewindTarget(entries, tc.row)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantHead, head)
			assert.Equal(t, tc.wantFill, fill)
		})
	}
}

// TestRewindTargetDropsReferenceReads pins the ordering @ expansion depends on:
// an injected read follows the message that asked for it, so rewinding onto that
// message leaves neither in the branch and a resubmit re-reads the file.
func TestRewindTargetDropsReferenceReads(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		sessionOnly("root"),
		pickAssistText("a0", "root", "earlier reply"),
		pickMsg("u1", "a0", llm.Text(llm.RoleUser, "explain @main.go")),
		pickToolCall("t1", "u1"),
		pickToolResultMsg("r1", "t1"),
		pickAssistText("a1", "r1", "it does X"),
	}

	head, fill, ok := RewindTarget(entries, "u1")
	require.True(t, ok)
	assert.Equal(t, "explain @main.go", fill)

	ids := make([]string, 0, len(entries))
	for _, e := range Branch(entries, head) {
		ids = append(ids, e.ID)
	}
	assert.Equal(t, []string{"root", "a0"}, ids)
}
