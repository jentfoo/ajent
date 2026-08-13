package command

import (
	"context"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
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
