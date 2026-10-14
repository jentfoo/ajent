package command

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

func TestSettingsSectionOpensDirectly(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// /settings reasoning jumps straight to the reasoning picker. Supported levels
	// are [off, minimal, low, medium, high], with index 4 selecting "high".
	c.picks = []fakePick{{result: 4}}
	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(t.Context(), "reasoning", c))
	assert.Equal(t, llm.LevelHigh, c.state.Reasoning.Level)
}

func TestSettingsReasoningCancelRecordsNothing(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// /settings reasoning with no queued pick: the picker cancels, and neither a
	// session override nor a save-to-layer prompt may appear for it
	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(t.Context(), "reasoning", c))
	_, srcName, _ := c.settings.Explain("reasoning.level")
	assert.NotEqual(t, "session", srcName)
	assert.Empty(t, c.selects) // the save prompt never opened
}

func TestSettingsReasoningUnavailableWithFewOptions(t *testing.T) {
	t.Parallel()

	// a model offering only off (no reasoning capability) locks the Reasoning row.
	c := newFakeConsole(t)
	reg, _ := llm.NewRegistry(llm.File{Providers: map[string]llm.ProviderConfig{
		"test": {APIKeyEnv: "TEST_API_KEY", Models: []llm.ModelConfig{{ID: "alpha"}}},
	}}, nil, llm.RegistryOptions{Env: func(string) string { return "" }})
	c.models = reg
	c.state.Model = reg.Active()

	assert.Equal(t, "only one level for this model", reasoningUnavailable(c))
}

func TestSettingsMenuCancelledClosesSilently(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// no queued pick: Pick returns ErrCancelled, the menu closes silently.
	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(t.Context(), "", c))
	assert.Empty(t, c.notices)
}

func TestSettingsUnknownSectionNotifies(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(t.Context(), "bogus", c))
	assert.True(t, c.noticeContains("no settings section"))
}

func TestSettingsRetentionRowEditsStateAndSaves(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// section jump lands on the retention row, Select picking "none" (index 0)
	c.selects = []int{0}

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(t.Context(), "reasoning retention", c))
	assert.Equal(t, llm.RetainNone, c.state.Reasoning.Retain)
	// the dotted leaf persists so a resume (and a save) keeps "none"
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

	// menu opens, pick the Auto-compaction row (index 5), Confirm on, Input threshold
	c.picks = []fakePick{{result: 5}}
	c.confirms = []bool{true}
	c.inputs = []string{"0.6"}

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(t.Context(), "", c))
	set := c.settings.Settings()
	assert.True(t, set.Compaction.Auto)
	assert.InDelta(t, 0.6, set.Compaction.Threshold, 1e-09)
}

func TestEditCompaction(t *testing.T) {
	t.Parallel()

	row := settingsRow{name: "Auto-compaction", render: rowCompaction, edit: editCompaction}

	tests := []struct {
		name  string
		input string
	}{
		{"valid_fraction_records_toggle_and_threshold", "0.6"},
		{"valid_absolute_records_token_count", "120000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeConsole(t)
			c.confirms = []bool{true}
			c.inputs = []string{tc.input}

			changes, err := row.edit(t.Context(), c)
			require.NoError(t, err)
			assert.Equal(t, "compaction.auto", changes[0].key)
			assert.Equal(t, true, changes[0].value)
			assert.Equal(t, "compaction.threshold", changes[1].key)
			// ParseFloat round-trips the literal exactly
			wantValue, err := strconv.ParseFloat(tc.input, 64)
			require.NoError(t, err)
			assert.InDelta(t, wantValue, changes[1].value.(float64), 0)
		})
	}
	t.Run("non_number_aborts_with_zero_changes", func(t *testing.T) {
		c := newFakeConsole(t)
		c.confirms = []bool{true}
		c.inputs = []string{"abc"}

		changes, err := row.edit(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
	})

	t.Run("out_of_range_aborts_with_zero_changes", func(t *testing.T) {
		c := newFakeConsole(t)
		c.confirms = []bool{true}
		c.inputs = []string{"0"}

		changes, err := row.edit(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
	})

	// NaN and ±Inf parse cleanly and pass naive range checks, so they must be
	// rejected explicitly before they can poison a live session setting
	t.Run("non_finite_aborts_with_zero_changes", func(t *testing.T) {
		for _, bad := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf"} {
			c := newFakeConsole(t)
			c.confirms = []bool{true}
			c.inputs = []string{bad}

			changes, err := row.edit(t.Context(), c)
			require.NoError(t, err)
			assert.Empty(t, changes)
			_, srcName, _ := c.settings.Explain("compaction.threshold")
			assert.NotEqual(t, "session", srcName) // nothing recorded
		}
	})
}

func TestSettingsModelRowHonoursSaveChoice(t *testing.T) {
	t.Parallel()

	// a save-to-project answer writes only that layer, never an automatic user one
	t.Run("saves_to_project_layer", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		// pick the Model row, applyModel picks beta, save prompt to project
		c.picks = []fakePick{{result: 0}, {result: 1}}
		c.selects = []int{2} // "save to project config"

		cmd, _ := r.Get("settings")
		require.NoError(t, cmd.Handler(t.Context(), "", c))

		require.Len(t, c.saveCalls, 1)
		assert.Equal(t, "project", c.saveCalls[0].layer)
		assert.Equal(t, "model", c.saveCalls[0].key)
	})

	// a session-only answer leaves every config layer untouched
	t.Run("session_only_writes_nothing", func(t *testing.T) {
		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		// pick the Model row, applyModel picks beta, save prompt to session only
		c.picks = []fakePick{{result: 0}, {result: 1}}
		c.selects = []int{0} // "this session only"

		cmd, _ := r.Get("settings")
		require.NoError(t, cmd.Handler(t.Context(), "", c))

		assert.Empty(t, c.saveCalls)
		// the switch still applies as a live session override
		v, src, ok := c.settings.Explain("model")
		require.True(t, ok)
		assert.Equal(t, "session", src)
		assert.JSONEq(t, `"test/beta"`, string(v))
	})
}

func TestEditReasoningRow(t *testing.T) {
	t.Parallel()

	// a pick records the level as a session override
	t.Run("pick_records_session_override", func(t *testing.T) {
		c := newFakeConsole(t)
		// supported levels are [off, minimal, low, medium, high]; index 4 picks high
		c.picks = []fakePick{{result: 4}}

		changes, err := editReasoning(t.Context(), c)
		require.NoError(t, err)
		assert.Equal(t, "reasoning.level", changes[0].key)
		assert.Equal(t, "high", changes[0].value)

		raw, srcName, ok := c.settings.Explain("reasoning.level")
		require.True(t, ok)
		assert.JSONEq(t, `"high"`, string(raw))
		assert.Equal(t, "session", srcName)
	})

	t.Run("cancelled_records_nothing", func(t *testing.T) {
		c := newFakeConsole(t)

		// no queued pick: Esc in the picker, nothing recorded
		changes, err := editReasoning(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
		_, srcName, _ := c.settings.Explain("reasoning.level")
		assert.NotEqual(t, "session", srcName)
	})

	// re-picking the active level is a no-op like any other pick that changes
	// nothing, so no override appears and the save prompt never opens
	t.Run("same_level_records_nothing", func(t *testing.T) {
		c := newFakeConsole(t)
		// the fake console starts at medium (index 3)
		c.picks = []fakePick{{result: 3}}

		changes, err := editReasoning(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
		_, srcName, _ := c.settings.Explain("reasoning.level")
		assert.NotEqual(t, "session", srcName)
	})

	// a model with only off never opens the picker; the /settings reasoning
	// section path skips the row's disabled check, so the editor must still
	// record nothing rather than pinning the current level
	t.Run("single_level_records_nothing", func(t *testing.T) {
		c := newFakeConsole(t)
		reg, _ := llm.NewRegistry(llm.File{Providers: map[string]llm.ProviderConfig{
			"test": {APIKeyEnv: "TEST_API_KEY", Models: []llm.ModelConfig{{ID: "alpha"}}},
		}}, nil, llm.RegistryOptions{Env: func(string) string { return "" }})
		c.models = reg
		c.state.Model = reg.Active()

		changes, err := editReasoning(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
		_, srcName, _ := c.settings.Explain("reasoning.level")
		assert.NotEqual(t, "session", srcName)
	})
}

func TestEditModelRow(t *testing.T) {
	t.Parallel()

	// a pick of the already-active model is a no-op like /model's unchanged key,
	// so no override appears and the save prompt never opens
	t.Run("same_model_records_nothing", func(t *testing.T) {
		c := newFakeConsole(t)
		// alpha is active; picker lists [alpha, beta], so index 0 re-picks it
		c.picks = []fakePick{{result: 0}}

		changes, err := editModel(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
		_, srcName, _ := c.settings.Explain("model")
		assert.NotEqual(t, "session", srcName)
	})

	// a different model records the override the save prompt persists
	t.Run("changed_model_records_override", func(t *testing.T) {
		c := newFakeConsole(t)
		c.picks = []fakePick{{result: 1}}

		changes, err := editModel(t.Context(), c)
		require.NoError(t, err)
		assert.Equal(t, "model", changes[0].key)
		assert.Equal(t, "test/beta", changes[0].value)

		raw, srcName, ok := c.settings.Explain("model")
		require.True(t, ok)
		assert.JSONEq(t, `"test/beta"`, string(raw))
		assert.Equal(t, "session", srcName)
	})
}

func TestEditToolsRow(t *testing.T) {
	t.Parallel()

	t.Run("cancelled_records_nothing", func(t *testing.T) {
		c := newFakeConsole(t)
		c.tools.Register(&fakeToolAdapter{name: "read"}, true)

		// no queued MultiPick: Esc in the picker, nothing recorded
		changes, err := editTools(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
		_, srcName, _ := c.settings.Explain("tools.enabled")
		assert.NotEqual(t, "session", srcName)
	})

	t.Run("widen_only_nothing_picked_records_nothing", func(t *testing.T) {
		c := newFakeConsole(t)
		c.tools.Register(&fakeToolAdapter{name: "read"}, true)
		c.tools.Register(&fakeToolAdapter{name: "ls"}, false)
		c.started = true // widen-only mode

		// confirming the picker with nothing selected enables nothing
		c.multiPicks = []fakeMultiPick{{result: []int{}}}
		changes, err := editTools(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
		_, srcName, _ := c.settings.Explain("tools.enabled")
		assert.NotEqual(t, "session", srcName)
	})

	// with no disabled tools the picker never opens; nothing was offered to pick
	t.Run("widen_only_all_enabled_records_nothing", func(t *testing.T) {
		c := newFakeConsole(t)
		c.tools.Register(&fakeToolAdapter{name: "read"}, true)
		c.started = true // widen-only mode

		changes, err := editTools(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
		_, srcName, _ := c.settings.Explain("tools.enabled")
		assert.NotEqual(t, "session", srcName)
	})

	// re-picking the currently enabled set leaves nothing to record
	t.Run("same_set_records_nothing", func(t *testing.T) {
		c := newFakeConsole(t)
		c.tools.Register(&fakeToolAdapter{name: "read"}, true)

		// free select lists [read]; picking just it reproduces the current set
		c.multiPicks = []fakeMultiPick{{result: []int{0}}}
		changes, err := editTools(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes)
		_, srcName, _ := c.settings.Explain("tools.enabled")
		assert.NotEqual(t, "session", srcName)
	})

	// a real change records the enabled set the save prompt persists
	t.Run("changed_set_records_override", func(t *testing.T) {
		c := newFakeConsole(t)
		c.tools.Register(&fakeToolAdapter{name: "read"}, true)
		c.tools.Register(&fakeToolAdapter{name: "ls"}, false)

		// free select: pick read and ls (indexes 0,1) => both enabled
		c.multiPicks = []fakeMultiPick{{result: []int{0, 1}}}
		changes, err := editTools(t.Context(), c)
		require.NoError(t, err)
		assert.Equal(t, "tools.enabled", changes[0].key)
		assert.Equal(t, []string{"read", "ls"}, changes[0].value)

		raw, srcName, ok := c.settings.Explain("tools.enabled")
		require.True(t, ok)
		assert.JSONEq(t, `["read","ls"]`, string(raw))
		assert.Equal(t, "session", srcName)
	})
}

func TestEnumRow(t *testing.T) {
	// not parallel: the edit case uses Setenv which cannot run alongside parallel siblings

	// a select records a session override, cancel leaving it untouched
	t.Run("edit_records_session_setting", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())

		c := newFakeConsole(t)
		r := enumRow("Permissions mode", "permissions.mode", []string{"allow-all", "auto"})

		// render shows the default until a value is set
		label, detail := r.render(c)
		assert.Equal(t, "Permissions mode", label)
		assert.Contains(t, detail, "default")

		// Select picks index 1 (auto), and the row records it as a session override
		c.selects = []int{1}
		changes, err := r.edit(t.Context(), c)
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

		// no queued Select: ErrCancelled, nothing recorded
		changes, err := r.edit(t.Context(), c)
		require.ErrorIs(t, err, tui.ErrCancelled)
		assert.Empty(t, changes)
		_, srcName, ok := c.settings.Explain("permissions.mode")
		assert.True(t, ok) // resolvable at the default layer now
		assert.NotEqual(t, "session", srcName)
	})
}

func TestModelRow(t *testing.T) {
	t.Parallel()

	// a pick records the sub-agent model under its own key
	t.Run("picks_and_records_subagent_key", func(t *testing.T) {
		c := newFakeConsole(t)
		r := modelRow("Sub-agent model", "subagent.model")

		// picker returns beta (index 1), and the row records it under its own key
		c.picks = []fakePick{{result: 1}}
		changes, err := r.edit(t.Context(), c)
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

		// no queued pick: ErrCancelled, nothing recorded
		changes, err := r.edit(t.Context(), c)
		require.ErrorIs(t, err, tui.ErrCancelled)
		assert.Empty(t, changes)
	})
}

func TestIntRowRecordsAndValidatesSubagentConcurrency(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := intRow("Sub-agent concurrency", "subagent.maxConcurrent", 1, 64)

	// a valid value persists under its own key
	c.inputs = []string{"6"}
	changes, err := r.edit(t.Context(), c)
	require.NoError(t, err)
	assert.Equal(t, "subagent.maxConcurrent", changes[0].key)
	assert.Equal(t, 6, changes[0].value)

	n, srcName, ok := c.settings.Explain("subagent.maxConcurrent")
	require.True(t, ok)
	var got int
	_ = json.Unmarshal(n, &got)
	assert.Equal(t, 6, got) // a number, not a string, so enumRow cannot store this
	assert.Equal(t, "session", srcName)
}

func TestIntRowRejectsOutOfRangeWithoutPersisting(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"0", "65", "abc"} {
		t.Run(bad, func(t *testing.T) {
			c := newFakeConsole(t)
			r := intRow("Sub-agent concurrency", "subagent.maxConcurrent", 1, 64)

			c.inputs = []string{bad}
			changes, err := r.edit(t.Context(), c)
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
		changes, err := r.edit(t.Context(), c)
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
			changes, err := r.edit(t.Context(), c)
			require.NoError(t, err)
			assert.Empty(t, changes)
			assert.True(t, c.noticeContains("must be between"))
			_, srcName, _ := c.settings.Explain("compaction.verbatimFraction")
			assert.NotEqual(t, "session", srcName)
		}
	})

	// NaN and ±Inf parse cleanly and pass naive range checks, so they must be
	// rejected explicitly
	t.Run("rejects_non_finite", func(t *testing.T) {
		for _, bad := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf"} {
			c := newFakeConsole(t)
			r := floatRow("Compaction verbatim size", "compaction.verbatimFraction", 0.01, 0.5)

			c.inputs = []string{bad}
			changes, err := r.edit(t.Context(), c)
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

	// jump to the Permissions mode row by name, Select picking allow-read (index 1)
	c.picks = []fakePick{{result: 0}}
	c.selects = []int{1}

	cmd, _ := r.Get("settings")
	require.NoError(t, cmd.Handler(t.Context(), "permissions mode", c))
	raw, srcName, ok := c.settings.Explain("permissions.mode")
	require.True(t, ok)
	assert.Equal(t, `"allow-read"`, string(raw))
	assert.Equal(t, "session", srcName)
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

func TestEditOverrides(t *testing.T) {
	// not parallel: the override setup uses Setenv

	findRow := func(tb testing.TB, name string) settingsRow {
		tb.Helper()
		for i := range allRows() {
			if allRows()[i].name == name {
				return allRows()[i]
			}
		}
		tb.Fatalf("settings row %q not found", name)
		return settingsRow{}
	}

	t.Run("unavailable_without_overrides", func(t *testing.T) {
		c := newFakeConsole(t)
		r := findRow(t, "Reset overrides")
		assert.Equal(t, "no session overrides", r.disabledReason(c))

		_, err := r.edit(t.Context(), c) // no queued Pick reads as cancelled
		require.ErrorIs(t, err, tui.ErrCancelled)
		assert.Empty(t, c.resets)
	})

	t.Run("reset_removes_one_override", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		c := newFakeConsole(t)
		require.NoError(t, c.settings.SetSession("model", "test/alpha"))

		r := findRow(t, "Reset overrides")
		assert.Empty(t, r.disabledReason(c))
		label, detail := r.render(c)
		assert.Equal(t, "Reset overrides", label)
		assert.Contains(t, detail, "1 this session")

		// item 0 is all-overrides, so item 1 is the model key itself
		c.picks = []fakePick{{result: 1}}
		changes, err := r.edit(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, changes) // a removal has nothing to persist

		_, _, ok := c.settings.Explain("model")
		assert.False(t, ok)
		assert.Equal(t, []string{"model"}, c.resets)
	})

	t.Run("all_overrides_reset_together", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		c := newFakeConsole(t)
		require.NoError(t, c.settings.SetSession("reasoning.level", "high"))
		require.NoError(t, c.settings.SetSession("tools.limits.bash.lines", 10))

		r := findRow(t, "Reset overrides")
		c.picks = []fakePick{{result: 0}}
		_, err := r.edit(t.Context(), c)
		require.NoError(t, err)

		assert.Empty(t, c.settings.SessionOverrides())
		assert.Equal(t, []string{"reasoning.level", "tools.limits.bash.lines"}, c.resets)
	})
}
