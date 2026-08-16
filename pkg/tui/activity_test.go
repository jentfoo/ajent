package tui

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestShadeRowPadsToFullWidth verifies an activity line is elided and padded to
// the terminal width inside its background shade, so a full-width bar results.
func TestShadeRowPadsToFullWidth(t *testing.T) {
	t.Parallel()
	th := NewTheme(Color256)

	const w = 41
	text := "sub-2  grep pattern" // 21 columns; the rest is trailing shade blanks
	short := shadeRow(th.Activity, text, w)
	assert.Equal(t, th.Activity.Open()+text+strings.Repeat(" ", w-displayWidth(text))+sgrReset,
		short, "shade spans edge to edge")
	assert.Zero(t, displayWidth(short)-w, "exactly one row wide")

	// over-long text elides yet still fills the width exactly
	trunc := shadeRow(th.Activity, strings.Repeat("x", 100), 20)
	assert.Zero(t, displayWidth(trunc)-20, "elided row is still full width")
}

// TestShadeRowNoColorIsPlain verifies a no-op theme falls back to an elided row.
func TestShadeRowNoColorIsPlain(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "sub-2  work", shadeRow(NewTheme(ColorNone).Activity, "sub-2  work", 40))
}

func TestUIActivity(t *testing.T) {
	t.Parallel()

	newUI := func(w, h int, in io.Reader) (*UI, *vt) {
		v := newVT(w, h)
		return newTestUI(t, v, in), v
	}

	t.Run("adds_ordered_rows", func(t *testing.T) {
		u, v := newUI(80, 12, strings.NewReader(""))

		u.SetActivity("agent-1", "running agent 1")
		u.SetActivity("agent-2", "running agent 2")

		screen := u.snapshot(v)
		assert.Contains(t, screen, "running agent 1")
		assert.Contains(t, screen, "running agent 2")
	})
	t.Run("same_key_replaces_in_place", func(t *testing.T) {
		u, v := newUI(80, 12, strings.NewReader(""))

		u.SetActivity("agent-1", "step one")
		u.SetActivity("agent-1", "step two")

		screen := u.snapshot(v)
		assert.Contains(t, screen, "step two")
		assert.NotContains(t, screen, "step one")
	})
	t.Run("empty_text_removes", func(t *testing.T) {
		u, v := newUI(80, 12, strings.NewReader(""))

		u.SetActivity("agent-1", "running agent 1")
		u.SetActivity("agent-1", "")

		assert.NotContains(t, u.snapshot(v), "running agent 1")
	})
	t.Run("empty_text_only_removes_its_key", func(t *testing.T) {
		u, v := newUI(80, 12, strings.NewReader(""))

		u.SetActivity("agent-1", "one")
		u.SetActivity("agent-2", "two")
		u.SetActivity("agent-1", "")

		screen := u.snapshot(v)
		assert.NotContains(t, screen, "one")
		assert.Contains(t, screen, "two")
	})
	t.Run("cap_shows_plus_n_more", func(t *testing.T) {
		u, v := newUI(80, 12, strings.NewReader(""))

		for i := range 8 {
			u.SetActivity(string(rune('a'+i)), "row "+string(rune('0'+i)))
		}

		screen := u.snapshot(v)
		assert.Contains(t, screen, "+5 more")
	})
	t.Run("elides_at_narrow_width", func(t *testing.T) {
		u, v := newUI(30, 12, strings.NewReader(""))

		u.SetActivity("agent-1", "a very long activity line that will not fit")

		screen := u.snapshot(v)
		assert.Contains(t, screen, "a very long activity")
		assert.NotContains(t, screen, "will not fit") // elided to the width
	})
	t.Run("never_reaches_committed_history", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.SetActivity("agent-1", "transient work")
		// committing a line flushes the live block; activity stays out of scrollback
		u.Notify("committed note", LevelInfo)

		assert.Contains(t, u.snapshot(v), "! committed note")
		for _, row := range v.scrollback {
			assert.NotContains(t, row, "transient work")
		}
	})
	t.Run("yields_first_on_a_short_terminal", func(t *testing.T) {
		// a two-row status plus the input minimum leaves no room for activity
		v := newVT(80, 3)
		u := newTestUI(t, v, strings.NewReader(""))
		u.SetStatusSegment(Segment{Key: "agents", Text: strings.Repeat("x", 200)})

		u.SetActivity("agent-1", "transient work")

		assert.NotContains(t, u.snapshot(v), "transient work")
	})
}

func TestUIActivityClearedOnReset(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	u := newTestUI(t, v, strings.NewReader(""))

	u.SetActivity("agent-1", "running agent 1")
	u.Reset()
	assert.NotContains(t, u.snapshot(v), "running agent 1")
}
