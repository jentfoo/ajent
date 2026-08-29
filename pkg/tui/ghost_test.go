package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStreamingCommitKeepsOutput guards against premature-scroll corruption in
// inline mode. Committing a completed markdown block mid-stream used to redraw the
// stale live preview (still holding those just-committed rows) below the new history;
// on a small terminal that ghost overflowed and pushed freshly committed lines into
// scrollback before they had been read.
func TestStreamingCommitKeepsOutput(t *testing.T) {
	t.Parallel()

	// the divider row costs one live-block line; height 7 keeps this on the same
	// overflow boundary that 6 held before chrome was added.
	v := newVT(20, 7)
	u := newTestUI(t, v, strings.NewReader(""))

	for _, c := range []string{
		"first paragraph line one",
		"\nsecond para lines here xxxxxx", // fills the small screen as a live preview
	} {
		u.Text(c)
	}
	assert.Empty(t, v.scrollback)

	// closing a block commits both paragraphs mid-stream
	u.Text("\n\nthird paragraph follows")

	var lost []string
	for _, l := range v.scrollback {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "first paragraph line") { // only the genuine overflow may scroll
			lost = append(lost, l)
		}
	}

	screen := u.snapshot(v)
	assert.Empty(t, lost,
		"committed output must not be prematurely scrolled away\nscreen:\n%s", screen)
	assert.Contains(t, screen, "second para")
}

// TestThinkingCommitKeepsOutput is the thinking mirror of the streaming guard:
// committing a completed reasoning line mid-stream used to redraw the stale live
// preview (still holding those just-committed rows) below the new history; on a
// small terminal that ghost overflowed and pushed freshly committed lines into
// scrollback before they had been read.
func TestThinkingCommitKeepsOutput(t *testing.T) {
	t.Parallel()

	v := newVT(20, 7)
	u := newTestUI(t, v, strings.NewReader(""))

	committed := []string{
		"✻ thinking",
		"first reasoning line one",
		"second para lines here xxxxxx",
	}
	// isRetired reports whether a scrolled row is a (possibly word-wrapped)
	// fragment of genuinely committed history, so only genuine overflow passes.
	isRetired := func(l string) bool {
		if strings.TrimSpace(l) == "" {
			return true
		}
		for _, c := range committed {
			if strings.Contains(c, l) {
				return true
			}
		}
		return false
	}

	for _, c := range []string{
		"first reasoning line one",
		"\nsecond para lines here xxxxxx", // fills the small screen as a live preview
	} {
		u.Thinking(c)
	}
	assert.Contains(t, u.snapshot(v), "second para", "the committed second line is on screen")

	// completing another line commits mid-stream while the preview still holds rows.
	// Only retired committed history may scroll; a leaked ghost/divider row would
	// carry text that was never part of those lines (see TestStreamingCommitKeepsOutput).
	u.Thinking("\nthird reasoning follows")

	var lost []string
	for _, l := range v.scrollback {
		if !isRetired(strings.TrimSpace(l)) {
			lost = append(lost, l)
		}
	}
	screen := u.snapshot(v)
	assert.Empty(t, lost,
		"nothing but retired committed history may scroll\nscreen:\n%s", screen)
	assert.Contains(t, screen, "second para", "the recently committed line remains on screen")
}
