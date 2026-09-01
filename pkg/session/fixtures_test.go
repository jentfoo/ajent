package session

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The committed corpus in testdata/branches. Every other test in this package
// writes a transcript and reads it back inside one run, so a renamed JSON field
// would move both sides together and pass; these fixtures were written by an
// earlier build and never change, which is what makes them able to fail.

func fixtureModel(key string) (llm.Model, error) {
	return llm.Model{Provider: "anthropic", ID: "claude-opus-4-5", ContextWindow: 200000}, nil
}

// tipIDs returns every chain-tip id in file order: entries whose id is not
// another entry's parent. The persisted head and each fork appear exactly once.
func tipIDs(entries []Entry) []string {
	parented := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.ParentID != "" {
			parented[e.ParentID] = true
		}
	}
	var out []string
	for _, e := range entries {
		if e.ID == "" || parented[e.ID] {
			continue
		}
		out = append(out, e.ID)
	}
	return out
}

// TestFixtureSchema pins the on-disk transcript format: the intricate part is
// the compaction entry, whose reduce plan is replayed on every rebuild.
func TestFixtureSchema(t *testing.T) {
	t.Parallel()

	t.Run("every_entry_still_decodes", func(t *testing.T) {
		for _, name := range []string{"tools.jsonl", "fork.jsonl", "compacted.jsonl"} {
			entries := loadEntries(t, name)

			var sd SessionData
			require.NoError(t, entries[0].Decode(&sd))
			assert.Equal(t, TypeSession, entries[0].Type)
			assert.Equal(t, Version(), sd.Version, "%s was written at the current schema version", name)

			for _, e := range entries {
				assert.NotEmpty(t, e.ID, name)
				assert.NotZero(t, e.TS, name)
			}
		}
	})

	t.Run("reduce_plan_round_trips", func(t *testing.T) {
		branch := loadBranch(t, "compacted.jsonl")

		var cd CompactionData
		var found bool
		for _, e := range branch {
			if e.Type == TypeCompaction {
				require.NoError(t, e.Decode(&cd))
				found = true
			}
		}
		require.True(t, found)

		assert.Equal(t, 4200, cd.Before)
		assert.Equal(t, 900, cd.After)
		assert.NotEmpty(t, cd.FirstKeptEntryID)
		require.NotNil(t, cd.Reduce)
		assert.Equal(t, []Stub{
			{CallID: "call_old", Text: "(elided: superseded read)"},
			{CallID: "call_5", Limit: 512},
		}, cd.Reduce.Stubs)
		assert.True(t, cd.Reduce.StripThinking)
		assert.Equal(t, Stats{Failed: 1, Superseded: 1, Truncated: 2, Aborted: 1, Summarized: 4}, cd.Reduce.Stats)

		// the drop list names a real entry, so replaying the plan has work to do
		require.Len(t, cd.Reduce.Drop, 1)
		assert.True(t, slices.ContainsFunc(branch, func(e Entry) bool {
			return e.ID == cd.Reduce.Drop[0]
		}))
	})

	t.Run("message_blocks_keep_their_types", func(t *testing.T) {
		branch := loadBranch(t, "tools.jsonl")
		msgs, warns := ContextMessages(branch, CompactionData{}, nil)
		require.Empty(t, warns)

		// the envelope has to carry each block back as its own concrete type
		assert.Equal(t, "assistant|thinking,text,tool_call|start by reading the file", digest(msgs)[1])
	})
}

// TestFixtureContextMessages asserts the whole assembled shape, so a message
// silently added or dropped fails rather than passing unnoticed.
func TestFixtureContextMessages(t *testing.T) {
	t.Parallel()

	t.Run("tool_heavy_branch", func(t *testing.T) {
		branch := loadBranch(t, "tools.jsonl")
		msgs, warns := ContextMessages(branch, CompactionData{}, nil)
		require.Empty(t, warns)

		lines := digest(msgs)
		require.Len(t, lines, 33)

		assert.Equal(t, []string{
			"user|text|review the parser and tidy it up",
			"assistant|thinking,text,tool_call|start by reading the file",
			"user|tool_result|package parse⏎// a long first read⏎line …",
			"assistant|tool_call|read {\"path\":\"parser.go\"}",
			"user|tool_result|package parse⏎// the second read⏎line 0 …",
			"assistant|tool_call|edit {\"path\":\"parser.go\"}",
			"user|tool_result|applied 1 hunk",
			"assistant|tool_call|write {\"path\":\"parser.go\"}",
			"user|tool_result|wrote parser.go",
			"assistant|tool_call|bash {\"cmd\":\"go build ./...\"}",
			"user|tool_result|exit 1: undefined: Foo⏎line 0 of capture…",
		}, lines[:11])

		// the rest is seven identical follow-up turns and the closing message
		tail := lines[11:]
		require.Len(t, tail, 22)
		for i := range 7 {
			n := i + 6
			assert.Equal(t, fmt.Sprintf("user|text|follow up %d", n), tail[i*3])
			assert.Equal(t, fmt.Sprintf("assistant|text,tool_call|Checking item %d.", n), tail[i*3+1])
			assert.Equal(t, "user|tool_result|line 0 of captured tool output, long eno…", tail[i*3+2])
		}
		assert.Equal(t, "assistant|text|All done: the parser is tidied.", tail[21])
	})

	t.Run("compaction_replays_the_cut", func(t *testing.T) {
		branch := loadBranch(t, "compacted.jsonl")

		var cd CompactionData
		for _, e := range branch {
			if e.Type == TypeCompaction {
				require.NoError(t, e.Decode(&cd))
			}
		}
		msgs, warns := ContextMessages(branch, cd, nil)
		require.Empty(t, warns)

		// everything before the cut collapses into the summary's user message
		assert.Equal(t, []string{
			"user|text|The conversation history before this poi…",
			"user|text|the kept question",
			"assistant|text|the kept answer",
		}, digest(msgs))
	})
}

// TestFixtureBranch walks the committed tree: one parent with two children is
// two independent lineages, not one log.
func TestFixtureBranch(t *testing.T) {
	t.Parallel()

	entries := loadEntries(t, "fork.jsonl")

	ids := tipIDs(entries)
	require.Len(t, ids, 2)

	seen := map[string][]string{}
	for _, id := range ids {
		branch := Branch(entries, id)
		msgs, warns := ContextMessages(branch, CompactionData{}, nil)
		require.Empty(t, warns)
		seen[digest(msgs)[len(msgs)-1]] = digest(msgs)
	}

	first, ok := seen["assistant|text|first answer"]
	require.True(t, ok)
	second, ok := seen["assistant|text|second answer"]
	require.True(t, ok)

	assert.Equal(t, []string{
		"user|text|shared question",
		"assistant|text|shared answer",
		"user|text|first path",
		"assistant|text|first answer",
	}, first)
	assert.Equal(t, []string{
		"user|text|shared question",
		"assistant|text|shared answer",
		"user|text|second path",
		"assistant|text|second answer",
	}, second)
}

// TestFixtureState rebuilds the agent state a resume would start from.
func TestFixtureState(t *testing.T) {
	t.Parallel()

	branch := loadBranch(t, "tools.jsonl")
	st, warns := State(branch, fixtureModel)
	require.Empty(t, warns)

	assert.Len(t, st.Messages, 33)
	assert.Equal(t, "claude-opus-4-5", st.Model.ID)
	assert.Equal(t, llm.ReasoningConfig{Level: llm.LevelMedium, Retain: llm.RetainLastTurn, Show: true},
		st.Reasoning)
}
