package command

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
)

// newPinnedConsole builds the fake console under a pinned empty home, so row
// values read the compiled defaults whatever the developer's own config says.
// Not parallel: the env pin forbids it.
func newPinnedConsole(t *testing.T) *fakeConsole {
	t.Helper()
	t.Setenv(config.EnvHome, t.TempDir())
	return newFakeConsole(t)
}

// rowByKey finds one row in a rows listing.
func rowByKey(t *testing.T, rows []SettingRow, key string) SettingRow {
	t.Helper()
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no settings row for %s", key)
	return SettingRow{}
}

func TestSettingRows(t *testing.T) {
	// not parallel: the pinned home forbids it

	t.Run("lists_rows_in_menu_order", func(t *testing.T) {
		c := newPinnedConsole(t)
		rows := SettingRows(c)
		keys := make([]string, 0, len(rows))
		for _, r := range rows {
			keys = append(keys, r.Key)
		}
		assert.Equal(t, []string{
			"model", "reasoning.level", "reasoning.retain", "reasoning.hide",
			"tools.enabled", "compaction.auto", "compaction.threshold",
			"compaction.minSteps", "compaction.verbatimFraction", "permissions.mode",
			"subagent.model", "subagent.maxConcurrent", "subagent.maxSteps",
			"images.block", "tools.limits.bash.lines", "tools.limits.bash.bytes",
			"tools.limits.read.lines", "tools.limits.read.bytes",
		}, keys)
	})

	t.Run("carries_value_and_source", func(t *testing.T) {
		c := newPinnedConsole(t)
		rows := SettingRows(c)
		block := rowByKey(t, rows, "images.block")
		assert.True(t, block.Editable)
		assert.JSONEq(t, "false", string(block.Value))
		assert.Equal(t, "default", block.Source)

		perm := rowByKey(t, rows, "permissions.mode")
		assert.JSONEq(t, `"allow-read"`, string(perm.Value))
		assert.Equal(t, "default", perm.Source)

		limits := rowByKey(t, rows, "tools.limits.bash.lines")
		assert.JSONEq(t, "4000", string(limits.Value))
	})

	t.Run("model_row_reports_active_when_unset", func(t *testing.T) {
		c := newPinnedConsole(t)
		rows := SettingRows(c)
		model := rowByKey(t, rows, "model")
		assert.Equal(t, "active", model.Source)
		assert.JSONEq(t, `"test/alpha"`, string(model.Value))
	})

	t.Run("unavailable_rows_carry_reason", func(t *testing.T) {
		c := newPinnedConsole(t)
		// a model offering a single reasoning level grays the level row
		file := llm.File{Providers: map[string]llm.ProviderConfig{
			"p": {Models: []llm.ModelConfig{{ID: "solo"}}},
		}}
		reg, _ := llm.NewRegistry(file, nil, llm.RegistryOptions{Env: func(string) string { return "" }})
		c.models = reg
		c.state.Model = reg.Active()

		rows := SettingRows(c)
		level := rowByKey(t, rows, "reasoning.level")
		assert.False(t, level.Editable)
		assert.Equal(t, "only one level for this model", level.Reason)
	})
}

func TestSetSetting(t *testing.T) {
	t.Parallel()

	t.Run("unknown_key", func(t *testing.T) {
		c := newFakeConsole(t)
		_, err := SetSetting(c, "nope.key", json.RawMessage("1"), "")
		assert.ErrorIs(t, err, ErrUnknownSetting)
	})

	t.Run("applies_session_override_and_live_effect", func(t *testing.T) {
		c := newFakeConsole(t)
		row, err := SetSetting(c, "images.block", json.RawMessage("true"), "")
		require.NoError(t, err)
		assert.True(t, c.settings.Settings().Images.Block)
		raw, src, ok := c.settings.Explain("images.block")
		require.True(t, ok)
		assert.JSONEq(t, "true", string(raw))
		assert.Equal(t, "session", src)
		assert.True(t, row.Editable)
		assert.Equal(t, "session", row.Source)
	})

	t.Run("value_shape_validated", func(t *testing.T) {
		c := newFakeConsole(t)
		for _, tc := range []struct {
			name string
			key  string
			raw  string
			want string
		}{
			{"bool_rejects_string", "images.block", `"yes"`, "boolean"},
			{"int_rejects_fraction", "compaction.minSteps", "2.5", "whole number"},
			{"int_rejects_above_range", "compaction.minSteps", "9", "between"},
			{"fraction_rejects_above_one", "compaction.verbatimFraction", "0.9", "between"},
			{"threshold_rejects_zero", "compaction.threshold", "0", "greater than zero"},
			{"mode_rejects_unknown", "permissions.mode", `"sometimes"`, "one of"},
			{"retain_rejects_unknown", "reasoning.retain", `"forever"`, "one of"},
			{"concurrency_rejects_zero", "subagent.maxConcurrent", "0", "between"},
			{"model_rejects_empty", "model", `""`, "non-empty"},
			{"limit_rejects_zero", "tools.limits.bash.lines", `0`, "at least"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := SetSetting(c, tc.key, json.RawMessage(tc.raw), "")
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.want)
			})
		}
	})

	t.Run("persist_copies_to_layer", func(t *testing.T) {
		c := newFakeConsole(t)
		_, err := SetSetting(c, "compaction.auto", json.RawMessage("false"), "user")
		require.NoError(t, err)
		require.Len(t, c.saveCalls, 1)
		assert.Equal(t, saveCall{layer: "user", key: "compaction.auto"}, c.saveCalls[0])

		_, err = SetSetting(c, "compaction.auto", json.RawMessage("false"), "local")
		require.ErrorContains(t, err, `persist must be "user" or "project"`)
		assert.Len(t, c.saveCalls, 1) // refused before anything applied or saved
	})

	t.Run("model_resolves_and_applies", func(t *testing.T) {
		c := newFakeConsole(t)
		row, err := SetSetting(c, "model", json.RawMessage(`"beta"`), "")
		require.NoError(t, err)
		assert.Equal(t, "test/beta", c.setModel.Key())
		assert.JSONEq(t, `"test/beta"`, string(row.Value))

		_, err = SetSetting(c, "model", json.RawMessage(`"missing"`), "")
		require.ErrorIs(t, err, llm.ErrUnknownModel)
	})

	t.Run("subagent_model_must_resolve", func(t *testing.T) {
		c := newFakeConsole(t)
		_, err := SetSetting(c, "subagent.model", json.RawMessage(`"beta"`), "")
		require.NoError(t, err)

		_, err = SetSetting(c, "subagent.model", json.RawMessage(`"missing"`), "")
		require.ErrorIs(t, err, llm.ErrUnknownModel)
		assert.NotContains(t, c.settings.SessionOverrides(), "subagent.model.missing")
	})

	t.Run("reasoning_fields_apply_live", func(t *testing.T) {
		c := newFakeConsole(t)
		_, err := SetSetting(c, "reasoning.retain", json.RawMessage(`"all"`), "")
		require.NoError(t, err)
		assert.Equal(t, llm.RetainAll, c.state.Reasoning.Retain)
		assert.Equal(t, llm.LevelMedium, c.state.Reasoning.Level) // untouched

		_, err = SetSetting(c, "reasoning.level", json.RawMessage(`"high"`), "")
		require.NoError(t, err)
		assert.Equal(t, llm.LevelHigh, c.state.Reasoning.Level)
		assert.Equal(t, llm.RetainAll, c.state.Reasoning.Retain) // untouched

		_, err = SetSetting(c, "reasoning.hide", json.RawMessage("true"), "")
		require.NoError(t, err)
		assert.True(t, c.state.Reasoning.Hide)
	})

	t.Run("unavailable_row_refuses", func(t *testing.T) {
		c := newFakeConsole(t)
		c.tools = nil
		_, err := SetSetting(c, "tools.enabled", json.RawMessage(`["read"]`), "")
		require.ErrorIs(t, err, ErrSettingUnavailable)
		assert.Contains(t, err.Error(), "no tools registered")
	})

	t.Run("tools_row_applies_through_toolset", func(t *testing.T) {
		c := newFakeConsole(t)
		c.tools.Register(&fakeToolAdapter{name: "read"}, true)
		c.tools.Register(&fakeToolAdapter{name: "ls"}, false)

		_, err := SetSetting(c, "tools.enabled", json.RawMessage(`["read","ls"]`), "")
		require.NoError(t, err)
		assert.Equal(t, []string{"read", "ls"}, c.tools.Names())
		assert.Equal(t, 1, c.toolsChanged)
	})

	t.Run("setting_needs_a_value", func(t *testing.T) {
		c := newFakeConsole(t)
		_, err := SetSetting(c, "images.block", nil, "")
		assert.ErrorContains(t, err, "needs a value")
	})
}

func TestSetSettingErrorSentinels(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	_, unknown := SetSetting(c, "nope", json.RawMessage("1"), "")
	require.ErrorIs(t, unknown, ErrUnknownSetting)

	c.tools = nil
	_, unavailable := SetSetting(c, "tools.enabled", json.RawMessage(`[]`), "")
	require.ErrorIs(t, unavailable, ErrSettingUnavailable)
}
