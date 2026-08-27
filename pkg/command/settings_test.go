package command

import (
	"context"
	"encoding/json"
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

	// /settings reasoning jumps straight to the reasoning picker. Supported levels
	// are [off, minimal, low, medium, high]; index 4 selects "high".
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

	// no queued pick: Pick returns ErrCancelled, the menu closes silently.
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

	// section jump lands on the retention row; Select picks "none" (index 0).
	c.selects = []int{0}

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
	assert.InDelta(t, 0.6, set.Compaction.Threshold, 1e-09)
}

func TestEditCompactionGathersThenApplies(t *testing.T) {
	t.Parallel()

	row := settingsRow{name: "Auto-compaction", render: rowCompaction, edit: editCompaction}

	t.Run("valid_fraction_records_toggle_and_threshold", func(t *testing.T) {
		c := newFakeConsole(t)
		c.confirms = []bool{true}
		c.inputs = []string{"0.6"}

		changes, err := row.edit(context.Background(), c)
		require.NoError(t, err)
		assert.Equal(t, "compaction.auto", changes[0].key)
		assert.Equal(t, true, changes[0].value)
		assert.Equal(t, "compaction.threshold", changes[1].key)
		assert.InDelta(t, 0.6, changes[1].value.(float64), 0) // exact; ParseFloat round-trips the literal
	})
}

func TestSettingsMenuSavesToProjectLayer(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// pick the Model row (0); modelCommand picks index 1 = beta; save prompt => project.
	c.picks = []fakePick{{result: 0}, {result: 1}}
	c.selects = []int{2} // "save to project config"

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(context.Background(), "", c))

	require.NotEmpty(t, c.saveCalls)
	assert.Equal(t, "project", c.saveCalls[0].layer)
	assert.Equal(t, "model", c.saveCalls[0].key)
}

// non-parallel: the edit case uses Setenv which cannot run alongside parallel siblings.
func TestEnumRow(t *testing.T) {
	// a select records a session override; cancel leaves it untouched.
	t.Run("edit_records_session_setting", func(t *testing.T) {
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
	})

	t.Run("cancelled_leaves_session_untouched", func(t *testing.T) {
		c := newFakeConsole(t)
		r := enumRow("Permissions mode", "permissions.mode", []string{"allow-all"})

		// no queued Select: ErrCancelled, nothing recorded.
		changes, err := r.edit(context.Background(), c)
		require.ErrorIs(t, err, tui.ErrCancelled)
		assert.Empty(t, changes)
		_, srcName, ok := c.settings.Explain("permissions.mode")
		assert.True(t, ok) // resolvable at the default layer now
		assert.NotEqual(t, "session", srcName)
	})
}

func TestModelRow(t *testing.T) {
	t.Parallel()

	// a pick records the sub-agent model under its own key.
	t.Run("picks_and_records_subagent_key", func(t *testing.T) {
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
	})

	t.Run("cancelled_returns_err", func(t *testing.T) {
		c := newFakeConsole(t)
		r := modelRow("Sub-agent model", "subagent.model")

		// no queued pick: ErrCancelled, nothing recorded.
		changes, err := r.edit(context.Background(), c)
		require.ErrorIs(t, err, tui.ErrCancelled)
		assert.Empty(t, changes)
	})
}

func TestIntRowRecordsAndValidatesSubagentConcurrency(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := intRow("Sub-agent concurrency", "subagent.maxConcurrent", 1, 64)

	// a valid value persists under its own key.
	c.inputs = []string{"6"}
	changes, err := r.edit(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, "subagent.maxConcurrent", changes[0].key)
	assert.Equal(t, 6, changes[0].value)

	n, srcName, ok := c.settings.Explain("subagent.maxConcurrent")
	require.True(t, ok)
	var got int
	_ = json.Unmarshal(n, &got)
	assert.Equal(t, 6, got) // a number, not a string; enumRow cannot store this
	assert.Equal(t, "session", srcName)
}

func TestIntRowRejectsOutOfRangeWithoutPersisting(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"0", "65", "abc"} {
		t.Run(bad, func(t *testing.T) {
			c := newFakeConsole(t)
			r := intRow("Sub-agent concurrency", "subagent.maxConcurrent", 1, 64)

			c.inputs = []string{bad}
			changes, err := r.edit(context.Background(), c)
			require.NoError(t, err)
			assert.Empty(t, changes)                            // invalid input aborts the edit
			assert.True(t, c.noticeContains("must be between")) // and explains why
			_, srcName, _ := c.settings.Explain("subagent.maxConcurrent")
			assert.NotEqual(t, "session", srcName) // nothing recorded
		})
	}
}

func TestFloatRowRecordsAndValidatesVerbatimFraction(t *testing.T) {
	t.Parallel()

	t.Run("edit_records_session_setting", func(t *testing.T) {
		c := newFakeConsole(t)
		r := floatRow("Compaction verbatim size", "compaction.verbatimFraction", 0.01, 0.5)

		c.inputs = []string{"0.25"}
		changes, err := r.edit(context.Background(), c)
		require.NoError(t, err)
		require.Len(t, changes, 1)
		assert.Equal(t, "compaction.verbatimFraction", changes[0].key)
		assert.InDelta(t, 0.25, changes[0].value, 1e-09)

		raw, srcName, ok := c.settings.Explain("compaction.verbatimFraction")
		require.True(t, ok)
		var got float64
		_ = json.Unmarshal(raw, &got)
		assert.InDelta(t, 0.25, got, 1e-09) // a number, not a string
		assert.Equal(t, "session", srcName)
	})

	t.Run("rejects_out_of_range", func(t *testing.T) {
		for _, bad := range []string{"0", "0.9", "nope"} {
			c := newFakeConsole(t)
			r := floatRow("Compaction verbatim size", "compaction.verbatimFraction", 0.01, 0.5)

			c.inputs = []string{bad}
			changes, err := r.edit(context.Background(), c)
			require.NoError(t, err)
			assert.Empty(t, changes)
			assert.True(t, c.noticeContains("must be between"))
			_, srcName, _ := c.settings.Explain("compaction.verbatimFraction")
			assert.NotEqual(t, "session", srcName)
		}
	})
}

func TestSettingsPermissionRowInMenu(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// jump to the Permissions mode row by name; Select picks allow-read (index 1).
	c.picks = []fakePick{{result: 0}}
	c.selects = []int{1}

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(context.Background(), "permissions mode", c))
	raw, srcName, ok := c.settings.Explain("permissions.mode")
	require.True(t, ok)
	assert.Equal(t, `"allow-read"`, string(raw))
	assert.Equal(t, "session", srcName)
}

func TestEditCompactionAbsoluteAndAborts(t *testing.T) {
	t.Parallel()

	row := settingsRow{name: "Auto-compaction", render: rowCompaction, edit: editCompaction}

	t.Run("valid_absolute_records_token_count", func(t *testing.T) {
		c := newFakeConsole(t)
		c.confirms = []bool{true}
		c.inputs = []string{"120000"}

		changes, err := row.edit(context.Background(), c)
		require.NoError(t, err)
		assert.Equal(t, "compaction.auto", changes[0].key)
		assert.InDelta(t, float64(120000), changes[1].value.(float64), 0) // exact; ParseFloat round-trips the literal
	})

	t.Run("non_number_aborts_with_zero_changes", func(t *testing.T) {
		c := newFakeConsole(t)
		c.confirms = []bool{true}
		c.inputs = []string{"abc"}

		changes, err := row.edit(context.Background(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
		_, srcName, _ := c.settings.Explain("compaction.threshold")
		assert.NotEqual(t, "session", srcName)
	})

	t.Run("out_of_range_aborts_with_zero_changes", func(t *testing.T) {
		c := newFakeConsole(t)
		c.confirms = []bool{true}
		c.inputs = []string{"0"}

		changes, err := row.edit(context.Background(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
		_, srcName, _ := c.settings.Explain("compaction.threshold")
		assert.NotEqual(t, "session", srcName)
	})
}

func TestRowCompactionRendersFractionAndAbsolute(t *testing.T) {
	t.Parallel()

	row := settingsRow{name: "Auto-compaction", render: rowCompaction, edit: editCompaction}

	c := newFakeConsole(t)
	_, detailDefault := row.render(c)
	assert.Contains(t, detailDefault, "%") // the default fraction renders as a percentage

	c = newFakeConsole(t)
	_ = c.SetSessionSetting("compaction.threshold", 0.6)
	_, detailFraction := row.render(c)
	assert.Contains(t, detailFraction, "60%")

	c = newFakeConsole(t)
	_ = c.SetSessionSetting("compaction.threshold", 120000)
	_, detailAbsolute := row.render(c)
	assert.Contains(t, detailAbsolute, "tokens")
	assert.NotContains(t, detailAbsolute, "%") // an absolute count must not read as a percentage
}
