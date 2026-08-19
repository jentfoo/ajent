package tui

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUIQueued covers the dimmed pending-prompt rows shown above the input while
// a turn runs: ordered rendering, the +N more cap, clearing, multi-line labels,
// and yielding on a short terminal like activity.
func TestUIQueued(t *testing.T) {
	t.Parallel()

	newUI := func(w, h int) (*UI, *vt) {
		v := newVT(w, h)
		return newTestUI(t, v, strings.NewReader("")), v
	}

	t.Run("renders_ordered_rows", func(t *testing.T) {
		u, v := newUI(80, 12)

		u.SetQueued([]string{"first queued", "second queued"})

		screen := u.snapshot(v)
		assert.Contains(t, screen, userMarker+"first queued")
		assert.Contains(t, screen, userMarker+"second queued")
	})
	t.Run("cap_shows_plus_n_more", func(t *testing.T) {
		u, v := newUI(80, 12)

		labels := make([]string, 0, 8)
		for i := range 8 {
			labels = append(labels, "row "+strconv.Itoa(i))
		}
		u.SetQueued(labels)

		assert.Contains(t, u.snapshot(v), "+5 more")
	})
	t.Run("nil_clears", func(t *testing.T) {
		u, v := newUI(80, 12)
		u.SetQueued([]string{"pending"})
		require.Contains(t, u.snapshot(v), "pending")

		u.SetQueued(nil)

		assert.NotContains(t, u.snapshot(v), "pending")
	})
	t.Run("first_line_only_of_multiline", func(t *testing.T) {
		u, v := newUI(80, 12)
		u.SetQueued([]string{"line one\nline two"})

		screen := u.snapshot(v)
		assert.Contains(t, screen, userMarker+"line one")
		assert.NotContains(t, screen, "line two")
	})
	t.Run("yields_on_short_terminal", func(t *testing.T) {
		v := newVT(80, 3)
		u := newTestUI(t, v, strings.NewReader(""))
		u.SetQueued([]string{"pending prompt"})

		assert.NotContains(t, u.snapshot(v), "pending prompt")
	})
}

// TestUIPrependInput covers inserting text at the top of the editor buffer,
// both on an empty buffer and ahead of a draft.
func TestUIPrependInput(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	u := newTestUI(t, v, strings.NewReader(""))

	t.Run("empty_buffer_sets", func(t *testing.T) {
		u.PrependInput("queued")
		u.mu.Lock()
		assert.Equal(t, "queued", u.editor.Value())
		u.mu.Unlock()
	})
	t.Run("non_empty_prepends_newline", func(t *testing.T) {
		v2 := newVT(40, 10)
		u2 := newTestUI(t, v2, strings.NewReader(""))
		u2.mu.Lock()
		u2.editor.SetValue("draft")
		u2.mu.Unlock()

		u2.PrependInput("queued")

		u2.mu.Lock()
		assert.Equal(t, "queued\ndraft", u2.editor.Value())
		u2.mu.Unlock()
	})
}

// TestUIAltUpEmitsRecallQueued pins Alt+↑ as an out-of-band Control so the front
// end can pop a queued prompt back into the editor.
func TestUIAltUpEmitsRecallQueued(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	pr, _ := io.Pipe() // stays open so the key loop never emits a spurious EOF
	u := newTestUI(t, v, pr)

	u.mu.Lock()
	submit, dirty, quit := u.applyKey(key{typ: keyAltUp})
	u.mu.Unlock()

	assert.Nil(t, submit)
	assert.False(t, dirty) // no repaint here; the driver updates via SetInput/PrependInput
	assert.False(t, quit)

	select {
	case c := <-u.Controls():
		assert.Equal(t, ControlRecallQueued, c)
	default:
		t.Fatal("expected a ControlRecallQueued emission")
	}
}
