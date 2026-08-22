package command

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/tui"
)

func TestThemeSetup(t *testing.T) {
	// the picker reads and writes config layers, so each case needs its own home

	t.Run("detected_tone_filters_options", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		c := newFakeConsole(t)
		c.tone = tui.ToneLight
		c.selects = []int{0}

		require.NoError(t, ThemeSetup(context.Background(), c))

		require.Len(t, c.selectOpts, 1)
		assert.Equal(t, paletteNames(tui.PalettesFor(tui.ToneLight)), optionLabels(c.selectOpts[0]))
		assert.Equal(t, "light", c.palette.Name)
	})
	t.Run("unknown_tone_offers_all", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		c := newFakeConsole(t)
		c.tone = tui.ToneUnknown
		c.selects = []int{0}

		require.NoError(t, ThemeSetup(context.Background(), c))

		require.Len(t, c.selectOpts, 1)
		assert.Equal(t, paletteNames(tui.Palettes()), optionLabels(c.selectOpts[0]))
	})
	t.Run("choice_is_recorded_and_saved", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		c := newFakeConsole(t)
		c.tone = tui.ToneDark
		c.selects = []int{1} // the second dark palette

		require.NoError(t, ThemeSetup(context.Background(), c))

		want := tui.PalettesFor(tui.ToneDark)[1].Name
		assert.Equal(t, want, c.palette.Name)
		value, src, ok := c.settings.Explain(themeKey)
		require.True(t, ok)
		assert.JSONEq(t, `"`+want+`"`, string(value))
		assert.Equal(t, "session", src)
		require.Len(t, c.saveCalls, 1)
		assert.Equal(t, saveCall{layer: "user", key: themeKey}, c.saveCalls[0])
	})
	t.Run("esc_persists_detected_match", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		c := newFakeConsole(t)
		c.tone = tui.ToneLight // no queued Select answer, so the picker is cancelled

		require.NoError(t, ThemeSetup(context.Background(), c))

		assert.Equal(t, tui.PalettesFor(tui.ToneLight)[0].Name, c.palette.Name)
		require.Len(t, c.saveCalls, 1) // saved, so the picker never returns
		assert.Equal(t, themeKey, c.saveCalls[0].key)
	})
	t.Run("esc_without_detection_uses_default", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		c := newFakeConsole(t)
		c.tone = tui.ToneUnknown

		require.NoError(t, ThemeSetup(context.Background(), c))

		assert.Equal(t, tui.DefaultPalette().Name, c.palette.Name)
	})
	t.Run("existing_choice_skips_picker", func(t *testing.T) {
		t.Setenv(config.EnvHome, t.TempDir())
		c := newFakeConsole(t)
		require.NoError(t, c.settings.SetSession(themeKey, "light"))

		require.NoError(t, ThemeSetup(context.Background(), c))

		assert.Empty(t, c.selectOpts)
		assert.Empty(t, c.palette.Name)
		assert.Empty(t, c.saveCalls)
	})
}

func TestThemeRow(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())

	c := newFakeConsole(t)
	row := themeRow()

	label, detail := row.render(c)
	assert.Equal(t, "Theme", label)
	assert.Contains(t, detail, "dark")
	assert.Contains(t, detail, "restart recolors history")

	idx := slices.IndexFunc(tui.Palettes(), func(p tui.Palette) bool { return p.Name == "light" })
	require.GreaterOrEqual(t, idx, 0)
	c.selects = []int{idx}
	changes, err := row.edit(context.Background(), c)
	require.NoError(t, err)

	require.Len(t, changes, 1)
	assert.Equal(t, settingChange{key: themeKey, value: "light"}, changes[0])
	assert.Equal(t, "light", c.palette.Name) // applied to the live UI, not just recorded
	_, src, ok := c.settings.Explain(themeKey)
	require.True(t, ok)
	assert.Equal(t, "session", src)
}

func TestPaletteSample(t *testing.T) {
	t.Parallel()

	t.Run("carries_palette_colors", func(t *testing.T) {
		pal, ok := tui.LookupPalette("light")
		require.True(t, ok)
		sample := paletteSample(tui.Color256, pal)

		th := tui.NewTheme(tui.Color256, pal)
		assert.Contains(t, sample, th.Prompt.Open())
		assert.Contains(t, sample, th.DiffAdd.Open())
		assert.Contains(t, sample, th.DiffDel.Open())
	})
	t.Run("plain_without_color", func(t *testing.T) {
		sample := paletteSample(tui.ColorNone, tui.DefaultPalette())
		assert.NotContains(t, sample, "\x1b")
		assert.Contains(t, sample, "prompt")
	})
}

// paletteNames returns the palette names in order.
func paletteNames(pals []tui.Palette) []string {
	names := make([]string, len(pals))
	for i, p := range pals {
		names[i] = p.Name
	}
	return names
}

// optionLabels returns the labels offered by one Select call.
func optionLabels(opts []tui.Option) []string {
	labels := make([]string, len(opts))
	for i, o := range opts {
		labels[i] = strings.TrimSpace(o.Label)
	}
	return labels
}
