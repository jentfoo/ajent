package command

import (
	"context"
	"testing"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsSectionOpensDirectly(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// /settings reasoning jumps straight to the reasoning picker. The fake's two
	// models both reason; supported levels start at off and index 3 is "high".
	c.picks = []fakePick{{result: 4}}
	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(context.Background(), "reasoning", c))
	assert.Equal(t, llm.LevelHigh, c.state.Reasoning.Level)
}

func TestSettingsMenuCancelledClosesSilently(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// no queued pick → Pick returns ErrCancelled; the menu closes with no notice.
	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(context.Background(), "", c))
	assert.Empty(t, c.notices)
}

func TestSettingsUnknownSectionNotifies(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(context.Background(), "bogus", c))
	assert.True(t, c.noticeContains("no settings section"))
}

func TestSettingsRetentionRowEditsStateAndSaves(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// menu opens on the retention row (2); Select picks "none" (index 0); save → user.
	c.picks = []fakePick{{result: 1}}
	c.selects = []int{0, 1}

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(context.Background(), "reasoning retention", c))
	assert.Equal(t, llm.RetainNone, c.state.Reasoning.Retain)
	// the dotted leaf persists so a resume (and a save) keeps "none".
	v, src, ok := c.settings.Explain("reasoning.retain")
	require.True(t, ok)
	assert.Equal(t, `"none"`, string(v))
	assert.Equal(t, "session", src)
}

func TestSettingsCompactionRowSetsSessionKeys(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// menu opens, pick the Auto-compaction row (index 5); Confirm on; Input threshold.
	c.picks = []fakePick{{result: 5}}
	c.confirms = []bool{true}
	c.inputs = []string{"0.6"}

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(context.Background(), "", c))
	set := c.settings.Settings()
	assert.True(t, set.Compaction.Auto)
}

func TestSettingsMenuSavesToProjectLayer(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// pick the Model row (0); modelCommand picks index 1 = beta; save prompt → project.
	c.picks = []fakePick{{result: 0}, {result: 1}}
	c.selects = []int{2} // "save to project config"

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(context.Background(), "", c))

	require.NotEmpty(t, c.saveCalls)
	assert.Equal(t, "project", c.saveCalls[0].layer)
	assert.Equal(t, "model", c.saveCalls[0].key)
}

// non-parallel: Setenv cannot run alongside parallel siblings, and the home dir
// is isolated so a real user config never shifts the rendered source away from default.
func TestEnumRowEditsAndRecordsSessionSetting(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())

	c := newFakeConsole(t)
	r := enumRow("Permissions mode", "permissions.mode", []string{"allow-all", "auto"})

	// render shows the default until a value is set.
	label, detail := r.render(c)
	assert.Equal(t, "Permissions mode", label)
	assert.Contains(t, detail, "default")

	// Select picks index 1 (auto); the row records it as a session override.
	c.selects = []int{1}
	changes, err := r.edit(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, "permissions.mode", changes[0].key)
	assert.Equal(t, "auto", changes[0].value)

	src, srcName, ok := c.settings.Explain("permissions.mode")
	require.True(t, ok)
	assert.Equal(t, `"auto"`, string(src))
	assert.Equal(t, "session", srcName)
}

func TestEnumRowCancelledLeavesSessionUntouched(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := enumRow("Permissions mode", "permissions.mode", []string{"allow-all"})

	// no queued Select → ErrCancelled; nothing is recorded.
	changes, err := r.edit(context.Background(), c)
	require.ErrorIs(t, err, tui.ErrCancelled)
	assert.Empty(t, changes)
	_, srcName, ok := c.settings.Explain("permissions.mode")
	assert.True(t, ok) // resolvable at the default layer now
	assert.NotEqual(t, "session", srcName)
}

func TestModelRowPicksAndRecordsSubagentKey(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := modelRow("Sub-agent model", "subagent.model")

	// picker returns beta (index 1); the row records it under its own key.
	c.picks = []fakePick{{result: 1}}
	changes, err := r.edit(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, "subagent.model", changes[0].key)
	assert.Equal(t, "test/beta", changes[0].value)

	raw, srcName, ok := c.settings.Explain("subagent.model")
	require.True(t, ok)
	assert.Equal(t, `"test/beta"`, string(raw))
	assert.Equal(t, "session", srcName)
}

func TestModelRowCancelledReturnsErr(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := modelRow("Sub-agent model", "subagent.model")

	// no queued pick → ErrCancelled, nothing recorded.
	changes, err := r.edit(context.Background(), c)
	require.ErrorIs(t, err, tui.ErrCancelled)
	assert.Empty(t, changes)
}

func TestSettingsPermissionRowInMenu(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// open the Permissions mode row (6); Select picks allow-read (index 1).
	c.picks = []fakePick{{result: 0}}
	c.selects = []int{1}

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(context.Background(), "permissions mode", c))
	raw, srcName, ok := c.settings.Explain("permissions.mode")
	require.True(t, ok)
	assert.Equal(t, `"allow-read"`, string(raw))
	assert.Equal(t, "session", srcName)
}
