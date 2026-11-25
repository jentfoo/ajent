package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPasteUI returns an inline rig with the given bodies already streaming in as
// bracketed pastes.
func newPasteUI(t *testing.T, bodies ...string) (*UI, *vt) {
	t.Helper()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	go func() {
		for _, body := range bodies {
			_, _ = io.WriteString(pw, "\x1b[200~"+body+"\x1b[201~")
		}
	}()
	return u, v
}

func TestBracketedPaste(t *testing.T) {
	t.Parallel()

	t.Run("large_stored_and_expanded", func(t *testing.T) {
		u, v := newPasteUI(t, strings.Repeat("line\n", 500))

		require.Eventually(t, func() bool {
			return strings.Contains(u.snapshot(v), "pasted")
		}, time.Second, testPoll)
		val := u.editorValue()
		assert.Contains(t, val, "pasted")

		// the paste content is stored and expandPastes recovers it whole
		u.mu.Lock()
		expanded := u.expandPastes(val)
		u.mu.Unlock()
		assert.Contains(t, expanded, "line\nline")
		assert.Len(t, expanded, 2500)
	})

	t.Run("placeholders_are_unique", func(t *testing.T) {
		first := strings.Repeat("aaaa\n", 500)
		second := strings.Repeat("bbbb\n", 500)
		u, _ := newPasteUI(t, first, second)

		require.Eventually(t, func() bool {
			u.mu.Lock()
			defer u.mu.Unlock()
			return len(u.pastes) == 2
		}, time.Second, testPoll)

		u.mu.Lock()
		expanded := u.expandPastes(u.editor.Value())
		placeholders := []string{u.pastes[0].placeholder, u.pastes[1].placeholder}
		u.mu.Unlock()

		assert.NotEqual(t, placeholders[0], placeholders[1])
		assert.Equal(t, first+second, expanded)
	})

	t.Run("small_inserted_inline", func(t *testing.T) {
		u, _ := newPasteUI(t, "short paste")

		require.Eventually(t, func() bool {
			return strings.Contains(u.editorValue(), "short paste")
		}, time.Second, testPoll)
		assert.NotContains(t, u.editorValue(), "pasted")
	})

	t.Run("crlf_stays_one_break", func(t *testing.T) {
		// Windows terminals deliver CRLF line endings inside a bracketed paste
		u, _ := newPasteUI(t, "first\r\nsecond")

		require.Eventually(t, func() bool {
			return u.editorValue() == "first\nsecond"
		}, time.Second, testPoll)
	})

	t.Run("stored_content_normalized", func(t *testing.T) {
		u, _ := newPasteUI(t, strings.Repeat("line\r\n", 500)) // well past pasteThreshold

		require.Eventually(t, func() bool {
			u.mu.Lock()
			defer u.mu.Unlock()
			return len(u.pastes) == 1
		}, time.Second, testPoll)

		u.mu.Lock()
		content := u.pastes[0].content
		expanded := u.expandPastes(u.editor.Value())
		u.mu.Unlock()

		assert.Equal(t, strings.Repeat("line\n", 500), content)
		assert.Equal(t, content, expanded)
	})
}
