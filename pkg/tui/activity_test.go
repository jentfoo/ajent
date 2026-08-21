package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShadeRowPadsToFullWidth verifies an activity line is elided and padded
// inside its background shade to one column short of the live width: the fill
// measures the sanitized string it draws, and the spare column absorbs a
// one-column width disagreement instead of wrapping the band.
func TestShadeRowPadsToFullWidth(t *testing.T) {
	t.Parallel()
	th := NewTheme(Color256)

	const w = 41
	text := "sub-2  grep pattern" // 21 columns; the rest is trailing shade blanks
	short := shadeRow(th.Activity, text, w)
	assert.Equal(t, th.Activity.Open()+text+strings.Repeat(" ", w-1-displayWidth(text))+sgrReset,
		short)
	assert.Zero(t, displayWidth(short)-(w-1))

	// over-long text elides yet still fills the target exactly
	trunc := shadeRow(th.Activity, strings.Repeat("x", 100), 20)
	assert.Zero(t, displayWidth(trunc)-19)
}

// TestShadeRowSanitizesCallerText pins that shadeRow measures the string it
// emits: a tab costs one column when drawn, so it must cost one when the fill
// is computed, and the emitted width must equal the width of its own
// sanitized form.
func TestShadeRowSanitizesCallerText(t *testing.T) {
	t.Parallel()
	th := NewTheme(Color256)

	const w = 24
	emitted := shadeRow(th.Activity, "a\t"+strings.Repeat("x", 15)+"\x1b[2B", w)
	assert.Zero(t, displayWidth(emitted)-(w-1))
	assert.Equal(t, displayWidth(sanitizeRow(emitted)), displayWidth(emitted))
	assert.NotContains(t, emitted, "\t")
	assert.NotContains(t, emitted, "\x1b[2B")
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

// TestUIActivityTabKeepsPark guards the shadeRow arithmetic against tabs:
// uniseg charges a tab one column while the terminal advances to the next
// 8-column stop, so an unmeasured tab makes the padded row wrap and the park
// lands one row short of the block's top.
func TestUIActivityTabKeepsPark(t *testing.T) {
	t.Parallel()

	v := newVT(20, 8)
	u := newTestUIWith(t, v, strings.NewReader(""), NewTheme(Color256))
	u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
	u.Print("hist")
	top, _ := u.cursor(v)

	u.SetActivity("k", "a\t"+strings.Repeat("x", 15))

	row, col := u.cursor(v)
	assert.Equal(t, top, row, "the tabbed row stays one terminal row")
	assert.Equal(t, 0, col)
	assert.Equal(t, 1, countRules(v.Screen()))
	assert.Contains(t, v.Screen(), "a "+strings.Repeat("x", 15), "the tab folded, nothing truncated")
}

// TestUIActivityEscapeKeepsRowCount drives a cursor-motion escape through the
// public activity API: unsanitized it moves the cursor rows nothing counted.
func TestUIActivityEscapeKeepsRowCount(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	u := newTestUIWith(t, v, strings.NewReader(""), NewTheme(Color256))
	u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
	u.Print("committed reply")
	top, _ := u.cursor(v)

	u.SetActivity("k", "work \x1b[2B in progress")

	row, _ := u.cursor(v)
	assert.Equal(t, top, row)
	assert.Equal(t, 1, countRules(v.Screen()))
	assert.Contains(t, v.Screen(), "work  in progress")

	v.setSize(24, 10)
	u.resize()
	assert.Equal(t, 1, countRules(v.Screen()), "the erase still finds the block's top")
	assert.Contains(t, v.Screen(), "committed reply", "history is untouched")
}

// TestUIPastedEscapeKeepsRowCount pins the boundary split: the editor buffer
// stays byte-exact (Value is what reaches the model), so an escape arrives in
// the live row and must be neutralized at the render boundary only.
func TestUIPastedEscapeKeepsRowCount(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	pr, pw := io.Pipe()
	u := newTestUI(t, v, pr)
	top, _ := u.cursor(v)

	_, err := io.WriteString(pw, "\x1b[200~sub \x1b[2B boom\x1b[201~")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return u.line(v, 1) == promptFirst+"sub  boom"
	}, time.Second, testPoll, "the pasted escape is neutralized on screen")

	u.mu.Lock()
	value := u.editor.Value()
	u.mu.Unlock()
	assert.Equal(t, "sub \x1b[2B boom", value, "the buffer keeps the paste byte-exact")

	row, _ := u.cursor(v)
	assert.Equal(t, top, row)
	assert.Equal(t, 1, countRules(v.Screen()))
	require.NoError(t, pw.Close())
}

func TestUIActivityClearedOnReset(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	u := newTestUI(t, v, strings.NewReader(""))

	u.SetActivity("agent-1", "running agent 1")
	u.Reset()
	assert.NotContains(t, u.snapshot(v), "running agent 1")
}
