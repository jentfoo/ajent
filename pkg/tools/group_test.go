package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistryUnitsCollapsesGroup verifies a registered group collapses its
// members into one row carrying every member name, sorted with builtins.
func TestRegistryUnitsCollapsesGroup(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"read", "write"} {
		r.Register(&fakeTool{name: n}, true)
	}
	for _, n := range []string{"agent_start", "agent_poll", "agent_list"} {
		r.RegisterFrom(SourceBuiltin, &fakeTool{name: n}, true)
	}
	r.RegisterGroup(ToolGroup{
		Name:   "subagents",
		Source: SourceBuiltin,
		Tools:  []string{"agent_start", "agent_poll", "agent_list"},
	})

	rows := r.Units(r.All())
	var plain [][]string
	for _, row := range rows {
		if row.Name == "subagents" { // the trio is one builtin row ahead of any MCP group
			assert.Equal(t, SourceBuiltin, row.Source)
			assert.ElementsMatch(t, []string{"agent_start", "agent_poll", "agent_list"}, row.Names)
		}
		if row.Name != "read" && row.Name != "write" {
			continue
		}
		plain = append(plain, row.Names)
	}
	assert.Len(t, plain, 2) // read and write remain their own rows
}

// TestRegistryUnitsPartialGroupFallsBack verifies a group with only some members
// offered (widen mode after a non-atomic change) falls back to per-member rows.
func TestRegistryUnitsPartialGroupFallsBack(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "ls"}, false)
	r.RegisterFrom(SourceBuiltin, &fakeTool{name: "agent_start"}, true) // enabled
	r.RegisterFrom(SourceBuiltin, &fakeTool{name: "agent_poll"}, false) // disabled
	r.RegisterGroup(ToolGroup{
		Name:   "subagents",
		Source: SourceBuiltin,
		Tools:  []string{"agent_start", "agent_poll"},
	})

	// only agent_poll is disabled and offered; the group is not fully present
	rows := r.Units(r.Disabled())
	require.Len(t, rows, 2) // ls + a lone agent_poll row
	var sawPoll bool
	for _, row := range rows {
		if row.Name == "agent_poll" { // offered individually, not as the group
			sawPoll = true
			assert.Equal(t, []string{"agent_poll"}, row.Names)
		}
	}
	assert.True(t, sawPoll)
}

// TestRegistryGroupTogglesTogether verifies enabling or replacing the enabled set
// through a group name flips every member at once.
func TestRegistryGroupTogglesTogether(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"read", "write"} {
		r.Register(&fakeTool{name: n}, true)
	}
	for _, n := range []string{"agent_start", "agent_poll", "agent_list"} {
		r.RegisterFrom(SourceBuiltin, &fakeTool{name: n}, false) // start disabled
	}
	r.RegisterGroup(ToolGroup{
		Name:   "subagents",
		Source: SourceBuiltin,
		Tools:  []string{"agent_start", "agent_poll", "agent_list"},
	})

	assert.False(t, r.enabled("agent_start"))

	r.Enable([]string{"subagents"}) // one label widens the whole trio
	for _, n := range []string{"agent_start", "agent_poll", "agent_list"} {
		assert.True(t, r.enabled(n))
	}

	// SetEnabled through a group name disables all members not selected again
	r.SetEnabled([]string{"read", "subagents"})
	for _, n := range []string{"agent_start", "agent_poll", "agent_list"} {
		assert.True(t, r.enabled(n)) // re-selected via the group label
	}
	assert.False(t, r.enabled("write")) // write was dropped

	r.SetEnabled([]string{"read"}) // no group name: every member goes off together
	for _, n := range []string{"agent_start", "agent_poll", "agent_list"} {
		assert.False(t, r.enabled(n))
	}
}

// TestRegistryUnitsInitialSelection verifies a fully-enabled group row counts as
// preselected (all members on), matching how /tools free-select marks it.
func TestRegistryUnitsRowNamesAndSource(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"agent_start", "agent_poll"} {
		r.RegisterFrom(SourceBuiltin, &fakeTool{name: n}, true)
	}
	r.RegisterGroup(ToolGroup{
		Name:   "subagents",
		Source: SourceBuiltin,
		Tools:  []string{"agent_start", "agent_poll"},
	})

	rows := r.Units(r.All())
	assert.Len(t, rows, 1) // only the collapsed group row
	row := rows[0]
	assert.Equal(t, "subagents", row.Name)
	assert.Equal(t, SourceBuiltin, row.Source)
	assert.ElementsMatch(t, []string{"agent_start", "agent_poll"}, row.Names)

	// a mixed member state still collapses when all are present in offered
	r.SetEnabled([]string{"agent_poll"})
	rows = r.Units(r.All())
	assert.Len(t, rows, 1)
}
