package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/tui"
)

// themeKey holds the chosen palette name.
const themeKey = "ui.theme"

// ThemeSetup offers the palette picker when no layer has chosen one yet, and
// persists the result so later starts skip it. Callers skip it in plain mode,
// which has no live block to preview a palette in.
func ThemeSetup(ctx context.Context, c Console) error {
	if c.Settings().Source(themeKey) != "default" {
		return nil // already chosen, or pinned by config, env or flag
	}
	tone := c.DetectTone()
	choices := tui.PalettesFor(tone)
	profile := c.ColorProfile()
	opts := bulk.SliceTransform(func(p tui.Palette) tui.Option {
		return tui.Option{Label: p.Name, Detail: paletteSample(profile, p)}
	}, choices)

	idx, err := c.Select(ctx, "Choose a color theme", opts)
	if err != nil {
		if !errorsIsCancelled(err) {
			return err
		}
		// Esc settles on the detected match, so a dismissed picker never returns
		applyTheme(c, fallbackPalette(tone))
		return nil
	}
	applyTheme(c, choices[idx])
	return nil
}

// themeRow is the /settings entry over every palette. It applies the choice to
// the live UI, which enumRow alone cannot do.
func themeRow() settingsRow {
	names := bulk.SliceTransform(func(p tui.Palette) string { return p.Name }, tui.Palettes())
	row := enumRow("Theme", themeKey, names)
	edit := row.edit
	row.edit = func(ctx context.Context, c Console) ([]settingChange, error) {
		changes, err := edit(ctx, c)
		for _, ch := range changes {
			if pal, ok := tui.LookupPalette(fmt.Sprint(ch.value)); ok {
				c.SetTheme(pal)
			}
		}
		return changes, err
	}
	row.render = func(c Console) (string, string) {
		return "Theme", detailOrDefault(c, themeKey) + " · restart recolors history"
	}
	return row
}

// applyTheme records the palette as a session override, recolors the live UI
// and saves it to the user layer.
func applyTheme(c Console, pal tui.Palette) {
	_ = c.SetSessionSetting(themeKey, pal.Name)
	c.SetTheme(pal)
	if err := c.SaveSetting("user", themeKey, pal.Name); err != nil {
		c.Notify("could not save theme: "+err.Error(), levelWarn)
	}
}

// fallbackPalette is the palette a dismissed picker settles on.
func fallbackPalette(tone tui.Tone) tui.Palette {
	if pals := tui.PalettesFor(tone); len(pals) > 0 {
		return pals[0]
	}
	return tui.DefaultPalette()
}

// paletteSample renders a strip of pal's roles in pal's own colors, so the
// picker shows real contrast instead of a name.
func paletteSample(p tui.ColorProfile, pal tui.Palette) string {
	t := tui.NewTheme(p, pal)
	return strings.Join([]string{
		t.Prompt.Wrap("❯ prompt"),
		t.Thinking.Wrap("✻ thinking"),
		t.Accent.Wrap("⏺ tool"),
		t.Code.Wrap("code"),
		t.DiffAdd.Wrap("+added"),
		t.DiffDel.Wrap("-removed"),
	}, " ")
}
