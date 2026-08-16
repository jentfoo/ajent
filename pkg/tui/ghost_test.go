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
