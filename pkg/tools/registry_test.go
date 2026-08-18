package tools

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (r *Registry) enabled(name string) bool {
	for _, rt := range r.tools {
		if rt.tool.Name() == name {
			return rt.state == StateEnabled
		}
	}
	return false
}

// newEditRegistry builds a registry holding an edit tool bound to dir.
func newEditRegistry(dir string) (*Registry, *Tracker) {
	tr := NewTracker()
	reg := New()
	reg.Register(&editTool{policy: PathPolicy{Cwd: dir}, tracker: tr}, true)
	return reg, tr
}

// editCall builds an edit call replacing old with new in path.
func editCall(path, old, new string) agent.ToolCall {
	args, _ := json.Marshal(editParams{Path: path, Edits: []editOp{{OldText: old, NewText: new}}})
	return agent.ToolCall{ID: "c", Name: "edit", Input: args}
}

// guardedEdit registers edit against a fresh registry and returns its guarded
// wrapper plus the env backing it.
func guardedEdit(t *testing.T) (agent.Tool, *toolEnv) {
	t.Helper()
	e := newToolEnv(t.TempDir())
	r := New()
	r.Register(&editTool{policy: e.policy, tracker: e.tracker}, true)
	tool, ok := r.Get("edit")
	require.True(t, ok)
	return tool, e
}

func TestRegistryDeclarationOrder(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"read", "write", "edit"} {
		r.Register(&fakeTool{name: n}, true)
	}
	assert.Equal(t, []string{"read", "write", "edit"}, r.Names())
	enabled := r.Enabled()
	got := make([]string, 0, len(enabled))
	for _, tool := range enabled {
		got = append(got, tool.Name())
	}
	assert.Equal(t, []string{"read", "write", "edit"}, got)
}

func TestRegistryDefaultEnableDisable(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "bash"}, true)
	r.Register(&fakeTool{name: "find"}, false)

	assert.True(t, r.enabled("bash"))
	assert.False(t, r.enabled("find"))

	_, ok := r.Get("bash")
	assert.True(t, ok) // enabled tools resolve
	_, ok = r.Get("find")
	assert.False(t, ok) // disabled ones do not
}

func TestRegistrySetEnabledWithUnknownNameIgnored(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"a", "b"} {
		r.Register(&fakeTool{name: n}, true)
	}
	r.SetEnabled([]string{"a", "ghost"}) // ghost is unknown and ignored
	assert.Equal(t, []string{"a"}, r.Names())
}

func TestRegistrySchemaCacheInvalidatedBySetEnabled(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"read", "write"} {
		r.Register(&fakeTool{name: n}, true)
	}
	schemas1 := r.Schemas()
	assert.Len(t, schemas1, 2)

	r.SetEnabled([]string{"write"}) // busts the cache
	schemas2 := r.Schemas()
	require.Len(t, schemas2, 1)
	assert.Equal(t, "write", schemas2[0].Name)
}

// TestRegistryEnablePromotesDisabled verifies Enable widens a disabled MCP tool to
// enabled so its schema reaches the prompt (the post-first-prompt /tools path).
func TestRegistryEnablePromotesDisabled(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"a", "b"} {
		r.RegisterState("mcp: srv", &fakeTool{name: "srv__" + n}, StateDisabled)
	}

	assert.Empty(t, r.Schemas())
	_, ok := r.Get("srv__a")
	assert.False(t, ok) // disabled tools are not callable by name

	r.Enable([]string{"srv__b"}) // promotion widens to enabled
	names := r.Names()
	require.Len(t, names, 1)
	assert.Equal(t, "srv__b", names[0])
	assert.Len(t, r.Schemas(), 1) // promoted schema now reaches the prompt
}

// TestRegistryMarkReadOnlyMetadata verifies read-only marking is queryable by
// name and dropped with the tool it belongs to.
func TestRegistryMarkReadOnlyMetadata(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"srv__a", "srv__b"} {
		r.RegisterFrom("mcp: srv", &fakeTool{name: n}, true)
	}
	assert.False(t, r.ReadOnly("srv__a")) // default is not read-only
	assert.False(t, r.ReadOnly("unknown"))

	r.MarkReadOnly([]string{"srv__b", "ghost"}) // ghost is unknown and ignored
	assert.True(t, r.ReadOnly("srv__b"))
	assert.False(t, r.ReadOnly("srv__a")) // unmarked stays false

	r.Unregister("mcp: srv") // the mark goes with its tool
	assert.False(t, r.ReadOnly("srv__b"))
}

// TestRegistryEnabledNamesBySource verifies EnabledNames scopes to a source.
func TestRegistryEnabledNamesBySource(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterFrom("mcp: x", &fakeTool{name: "x__1"}, true)
	r.RegisterFrom("mcp: x", &fakeTool{name: "x__2"}, false)
	r.Register(&fakeTool{name: "read"}, true)

	names := r.EnabledNames("mcp: x")
	assert.Equal(t, []string{"x__1"}, names) // only the enabled one from that source
}

// TestRegistryLookupIgnoresEnabled asserts Lookup returns a tool regardless of
// enabled state, while Get respects it — the silent bug picking the wrong one
// causes is what separate methods prevent.
func TestRegistryLookupIgnoresEnabled(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "ls"}, false)

	_, ok := r.Get("ls")
	assert.False(t, ok) // Get refuses a disabled tool

	got, ok := r.Lookup("ls")
	require.True(t, ok)
	assert.Equal(t, "ls", got.Name())

	_, ok = r.Lookup("ghost")
	assert.False(t, ok)
}

// TestRegistryDisabledListsDisabledOnly asserts Disabled returns the disabled
// tools in declaration order, mirroring Enabled's contract.
func TestRegistryDisabledListsDisabledOnly(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "read"}, true)
	r.Register(&fakeTool{name: "ls"}, false)
	r.Register(&fakeTool{name: "find"}, false)

	disabled := r.Disabled()
	got := make([]string, 0, len(disabled))
	for _, tool := range disabled {
		got = append(got, tool.Name())
	}
	assert.Equal(t, []string{"ls", "find"}, got)
}

// TestRegistryEnableIsAdditive asserts Enable only widens the set, leaving
// already-enabled tools on, unlike SetEnabled which replaces wholesale.
func TestRegistryEnableIsAdditive(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "read"}, true)
	r.Register(&fakeTool{name: "ls"}, false)

	r.Enable([]string{"ls"})
	assert.True(t, r.enabled("read")) // Enable must not disable existing tools
	assert.True(t, r.enabled("ls"))

	r.Enable([]string{"ghost"}) // unknown names ignored, no error
	assert.True(t, r.enabled("ls"))
}

// TestRegistrySourceLabelsGroups asserts RegisterFrom records a source label and
// Source returns it, while Register defaults to builtin.
func TestRegistrySourceLabelsGroups(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "read"}, true)
	r.RegisterFrom("filesystem-server", &fakeTool{name: "stat"}, false)

	assert.Equal(t, SourceBuiltin, r.Source("read"))
	assert.Equal(t, "filesystem-server", r.Source("stat"))
	assert.Empty(t, r.Source("ghost"))
}

// TestRegistryTrackerExposedByBuiltins asserts Builtins wires the shared tracker
// so @-expansion can dedupe through it.
func TestRegistryTrackerExposedByBuiltins(t *testing.T) {
	t.Parallel()

	reg, err := Builtins(Options{Cwd: t.TempDir(), SessionID: "t"})
	require.NoError(t, err)
	assert.NotNil(t, reg.Tracker())

	// a bare Registry has no tracker, so callers check for nil
	empty := New()
	assert.Nil(t, empty.Tracker())
}

func TestRegistryDryRunDispatchesToTool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	e := newToolEnv(dir)
	e.writeFile("a.txt", "hello\n")
	reg, _ := newEditRegistry(dir)

	c := agent.ToolCall{ID: "c", Name: "edit", Input: json.RawMessage(`{"path":"a.txt","edits":[{"oldText":"missing","newText":"x"}]}`)}
	require.Error(t, reg.DryRun(c)) // edit implements DryRunner; a doomed call errors

	c.Input = json.RawMessage(`{"path":"a.txt","edits":[{"oldText":"hello","newText":"hi"}]}`)
	require.NoError(t, reg.DryRun(c))
}

func TestRegistryDryRunNilForNonDryTool(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	reg := New()
	reg.Register(&readTool{policy: e.policy, tracker: e.tracker}, true)

	c := agent.ToolCall{ID: "c", Name: "read", Input: json.RawMessage(`{"path":"x"}`)}
	assert.NoError(t, reg.DryRun(c)) // cannot predict; never skip a prompt on uncertainty
}

func TestRegistryDryRunNilForUnknownTool(t *testing.T) {
	t.Parallel()

	reg := New()
	c := agent.ToolCall{ID: "c", Name: "nope", Input: json.RawMessage(`{}`)}
	assert.NoError(t, reg.DryRun(c))
}

// TestLsRegisteredDisabledInBuiltins asserts the off-by-default extras are not
// offered until enabled.
func TestLsRegisteredDisabledInBuiltins(t *testing.T) {
	t.Parallel()

	reg, err := Builtins(Options{Cwd: t.TempDir(), SessionID: "test"})
	require.NoError(t, err)
	assert.NotContains(t, reg.Names(), "ls") // off by default like find and grep
	assert.NotContains(t, reg.Names(), "find")
	assert.NotContains(t, reg.Names(), "grep")

	reg.SetEnabled([]string{"read", "ls"})
	assert.Contains(t, reg.Names(), "ls")
}

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

// TestRegistryUnitsRowNamesAndSource verifies a fully-enabled group row collapses
// to one row carrying the source and member names.
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

// TestGuardedToolPreviewOrdering pins when the change is rendered: before the
// guard chain runs, so an approval dialog opens below the full diff.
func TestGuardedToolPreviewOrdering(t *testing.T) {
	t.Parallel()

	t.Run("renders_before_the_guard", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "hello world\n")
		e.readExec(t.Context(), `{"path":"a.txt"}`)

		r := New()
		r.Register(&editTool{policy: e.policy, tracker: e.tracker}, true)
		dc := &diffCatcher{}
		var diffsAtGuard int
		r.AddGuard(func(context.Context, agent.ToolCall) Decision {
			diffsAtGuard = len(dc.calls)
			return Allow(agent.ToolCall{})
		})
		tool, ok := r.Get("edit")
		require.True(t, ok)

		res, err := tool.Execute(t.Context(), editCall("a.txt", "world", "ajent"), dc)
		require.NoError(t, err)
		assert.False(t, res.IsError)

		assert.Equal(t, 1, diffsAtGuard) // already rendered by the time the guard ran
		require.Len(t, dc.calls, 1)      // and not a second time from Execute
		assert.Equal(t, "hello world\n", dc.last().Before)
		assert.Equal(t, "hello ajent\n", dc.last().After)
	})

	t.Run("renders_even_when_denied", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "hello world\n")
		e.readExec(t.Context(), `{"path":"a.txt"}`)

		r := New()
		r.Register(&editTool{policy: e.policy, tracker: e.tracker}, true)
		r.AddGuard(func(context.Context, agent.ToolCall) Decision { return Deny("nope") })
		tool, ok := r.Get("edit")
		require.True(t, ok)

		dc := &diffCatcher{}
		res, err := tool.Execute(t.Context(), editCall("a.txt", "world", "ajent"), dc)
		require.NoError(t, err)
		assert.True(t, res.IsError) // the record shows what was proposed and refused
		require.Len(t, dc.calls, 1)
		assert.Equal(t, "hello ajent\n", dc.last().After)

		got, readErr := os.ReadFile(e.policy.Cwd + "/a.txt")
		require.NoError(t, readErr)
		assert.Equal(t, "hello world\n", string(got)) // nothing touched disk
	})

	t.Run("renders_once_when_asker_reasks", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "hello world\n")
		e.readExec(t.Context(), `{"path":"a.txt"}`)

		r := New()
		r.Register(&editTool{policy: e.policy, tracker: e.tracker}, true)
		r.AddGuard(func(context.Context, agent.ToolCall) Decision {
			return Decision{Action: ActionAsk, Reason: "approval"}
		})
		r.SetAsker(func(context.Context, agent.ToolCall, Decision) Decision {
			return Decision{Action: ActionAsk}
		})
		tool, ok := r.Get("edit")
		require.True(t, ok)

		dc := &diffCatcher{}
		_, err := tool.Execute(t.Context(), editCall("a.txt", "world", "ajent"), dc)
		require.NoError(t, err)
		assert.Len(t, dc.calls, 1) // rendered once despite the re-ask
	})

	t.Run("bad_args_render_nothing", func(t *testing.T) {
		tool, _ := guardedEdit(t)
		dc := &diffCatcher{}
		c := agent.ToolCall{ID: "c", Name: "edit", Input: json.RawMessage(`{"path":`)}
		res, err := tool.Execute(t.Context(), c, dc)
		require.NoError(t, err)
		assert.True(t, res.IsError)
		assert.Empty(t, dc.calls)
	})

	t.Run("nil_output_is_safe", func(t *testing.T) {
		tool, e := guardedEdit(t)
		e.writeFile("a.txt", "hello world\n")
		e.readExec(t.Context(), `{"path":"a.txt"}`)
		_, err := tool.Execute(t.Context(), editCall("a.txt", "world", "ajent"), nil)
		require.NoError(t, err)
	})
}

// TestGuardedToolPreviewSkipsNonPreviewers asserts a tool without a Preview runs
// untouched and renders nothing.
func TestGuardedToolPreviewSkipsNonPreviewers(t *testing.T) {
	t.Parallel()

	r := New()
	inner := &recordingTool{}
	r.Register(inner, true)
	tool, ok := r.Get("bash")
	require.True(t, ok)

	dc := &diffCatcher{}
	_, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), dc)
	require.NoError(t, err)
	assert.True(t, inner.done)
	assert.Empty(t, dc.calls)
}
