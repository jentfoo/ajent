package llm

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLevelValue(t *testing.T) {
	t.Parallel()

	var caps Capabilities

	t.Run("absent_key_uses_level_name", func(t *testing.T) {
		e, ok := levelValue(caps, LevelHigh)
		assert.True(t, ok)
		assert.Equal(t, "high", e)
	})
	t.Run("mapped_value_is_sent", func(t *testing.T) {
		c := caps
		v := "x-high"
		c.LevelMap = map[Level]*string{LevelXHigh: &v}
		e, ok := levelValue(c, LevelXHigh)
		assert.True(t, ok)
		assert.Equal(t, "x-high", e)
	})
	t.Run("null_entry_reports_unsupported", func(t *testing.T) {
		c := caps
		c.LevelMap = map[Level]*string{LevelMax: nil}
		e, ok := levelValue(c, LevelMax)
		assert.False(t, ok)
		assert.Empty(t, e)
	})
	t.Run("never_substitutes_high", func(t *testing.T) {
		// an unmapped xhigh/max must not silently become "high"
		c := caps
		e, _ := levelValue(c, LevelXHigh)
		assert.Equal(t, "xhigh", e)
	})
}

func TestOffValueAndSuppressed(t *testing.T) {
	t.Parallel()

	var caps Capabilities

	t.Run("absent_off_uses_default", func(t *testing.T) {
		e, ok := offValue(caps, "none")
		assert.True(t, ok)
		assert.Equal(t, "none", e)
	})
	t.Run("empty_default_reports_false", func(t *testing.T) {
		_, ok := offValue(caps, "")
		assert.False(t, ok)
	})
	t.Run("null_off_is_suppressed", func(t *testing.T) {
		c := caps
		c.LevelMap = map[Level]*string{LevelOff: nil}
		e, ok := offValue(c, "none")
		assert.False(t, ok)
		assert.Empty(t, e)
		assert.True(t, offSuppressed(c))
	})
	t.Run("mapped_off_value_sent", func(t *testing.T) {
		c := caps
		v := "disabled"
		c.LevelMap = map[Level]*string{LevelOff: &v}
		e, ok := offValue(c, "none")
		assert.True(t, ok)
		assert.Equal(t, "disabled", e)
		assert.False(t, offSuppressed(c))
	})
}

func TestLevelsFor(t *testing.T) {
	t.Parallel()

	t.Run("non_reasoning_offers_only_off", func(t *testing.T) {
		c := Capabilities{Reasoning: false}
		assert.Equal(t, []Level{LevelOff}, levelsFor(c))
	})
	t.Run("all_levels_by_default", func(t *testing.T) {
		c := Capabilities{Reasoning: true}
		// xhigh and max are opt-in, so the default ladder stops at high
		assert.Equal(t, []Level{LevelOff, LevelMinimal, LevelLow, LevelMedium, LevelHigh},
			levelsFor(c))
	})
	t.Run("null_entry_removes_a_level", func(t *testing.T) {
		c := Capabilities{Reasoning: true}
		v := "on"
		c.LevelMap = map[Level]*string{LevelHigh: nil, LevelXHigh: &v, LevelMax: &v}
		got := levelsFor(c)
		assert.NotContains(t, got, LevelHigh)
		assert.Contains(t, got, LevelXHigh)
		assert.Len(t, got, 6) // off..medium plus xhigh and max
	})
	t.Run("xhigh_and_max_are_opt_in", func(t *testing.T) {
		c := Capabilities{Reasoning: true}
		v := "on"
		c.LevelMap = map[Level]*string{LevelXHigh: &v, LevelMax: &v}
		got := levelsFor(c)
		assert.Contains(t, got, LevelXHigh)
		assert.Contains(t, got, LevelMax)
	})
	t.Run("off_null_hides_off", func(t *testing.T) {
		c := Capabilities{Reasoning: true}
		c.LevelMap = map[Level]*string{LevelOff: nil}
		assert.NotContains(t, levelsFor(c), LevelOff)
	})
}

func TestClampLevel(t *testing.T) {
	t.Parallel()

	full := Capabilities{Reasoning: true}

	t.Run("supported_level_unchanged", func(t *testing.T) {
		assert.Equal(t, LevelMedium, clampLevel(full, LevelMedium))
	})
	t.Run("off_stays_off_even_when_suppressed", func(t *testing.T) {
		c := Capabilities{Reasoning: true}
		c.LevelMap = map[Level]*string{LevelOff: nil}
		assert.Equal(t, LevelOff, clampLevel(c, LevelOff))
	})
	t.Run("xhigh_absent_clamps_down_to_high", func(t *testing.T) {
		assert.Equal(t, LevelHigh, clampLevel(full, LevelMax)) // max opt-in -> down to high
	})
	t.Run("restricted_map_snaps_upward_first", func(t *testing.T) {
		c := Capabilities{Reasoning: true}
		v := "on"
		c.LevelMap = map[Level]*string{LevelMinimal: &v, LevelLow: &v,
			LevelMedium: nil, LevelHigh: nil, LevelXHigh: nil, LevelMax: nil}
		assert.Equal(t, LevelOff, clampLevel(c, LevelOff))
		assert.Equal(t, LevelMinimal, clampLevel(c, LevelMinimal)) // supported stays
	})
	t.Run("xhigh_only_map", func(t *testing.T) {
		c := Capabilities{Reasoning: true}
		v := "on"
		c.LevelMap = map[Level]*string{LevelXHigh: &v, LevelMax: nil, LevelOff: nil}
		assert.Equal(t, LevelMedium, clampLevel(c, LevelMedium)) // medium supported
		assert.Equal(t, LevelXHigh, clampLevel(c, LevelMax))     // down from max
	})
}

func TestMaxOutputFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		m        Model
		input    int
		expected int
	}{
		// a cap smaller than the available window is kept as-is
		{"model_cap_under_window", Model{ContextWindow: 100000, MaxOutput: 8000}, 3000, 8000},
		// window - used - reserve(20%) leaves response headroom below the cap
		{"near_full_clamps_to_reserve", Model{ContextWindow: 200000, MaxOutput: 64000}, 157000, 3000},
		// an unknown window falls back to the model's own output cap
		{"unknown_window_uses_model_cap", Model{MaxOutput: 32000}, 9999, 32000},
		// no cap so the available window (minus reserve) is the output ceiling
		{"unset_cap_uses_window", Model{ContextWindow: 100000}, 3000, 77000},
		{"floored_at_one_token", Model{ContextWindow: 200000, MaxOutput: 64000}, 400000, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, MaxOutputFor(tc.m, tc.input))
		})
	}
}

func TestLevelsAreSortedAscending(t *testing.T) {
	t.Parallel()

	caps := Capabilities{Reasoning: true}
	got := levelsFor(caps)
	assert.True(t, slices.IsSorted(got))
}
