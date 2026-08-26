package session

import (
	"slices"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
)

func tipIDs(entries []Entry) []string {
	tips := Tips(entries)
	out := make([]string, len(tips))
	for i, tp := range tips {
		out[i] = tp.ID
	}
	return out
}

func TestTips(t *testing.T) {
	t.Parallel()

	// a linear chain then a fork: only true branch tips appear, with the first user prompt on each.
	t.Run("linear_and_fork", func(t *testing.T) {
		var entries []Entry
		add := func(id, parent string, e Entry) {
			e.ID = id
			e.ParentID = parent
			entries = append(entries, e)
		}
		session := Entry{Type: TypeSession}
		add("root", "", session)
		add("a", "root", userTextMsg(llm.RoleUser, "first question"))
		add("b", "a", userTextMsg(llm.RoleAssistant, "an answer"))

		// a single linear chain yields exactly its tail as the only tip.
		assert.Equal(t, []string{"b"}, tipIDs(entries))

		forked := slices.Clone(entries)
		// a second fork growing from the root
		fork := userTextMsg(llm.RoleUser, "branch from the root")
		fork.ID = "c"
		fork.ParentID = "root"
		forked = append(forked, fork)
		tip2 := userTextMsg(llm.RoleAssistant, "fork reply")
		tip2.ID = "d"
		tip2.ParentID = "c"
		forked = append(forked, tip2)

		// both the original chain and the new fork keep their own tips.
		assert.Equal(t, []string{"b", "d"}, tipIDs(forked))

		tips := Tips(forked)
		assert.Contains(t, tips[0].First, "first question")
		assert.Contains(t, tips[1].First, "branch from the root")
	})

	// an empty transcript returns nothing; a lone session root is one reachable tip.
	t.Run("empty", func(t *testing.T) {
		assert.Empty(t, Tips(nil))
		// a transcript with only the session root is itself one reachable tip.
		assert.Equal(t, []string{"root"}, tipIDs([]Entry{{ID: "root", Type: TypeSession}}))
	})
}

// userTextMsg builds a message entry carrying one text block of the given role.
func userTextMsg(role llm.Role, text string) Entry {
	return Entry{Type: TypeMessage, Data: mustJSON(MessageData{Message: llm.Text(role, text)})}
}
