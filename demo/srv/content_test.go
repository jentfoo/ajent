package srv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNotesGoConsistent guards the edit choreography: step 1 writes notes.go, so
// it must carry retryBefore verbatim (step 4's oldText) and not yet retryAfter.
func TestNotesGoConsistent(t *testing.T) {
	t.Parallel()
	assert.Contains(t, notesGo, retryBefore)
	assert.NotContains(t, notesGo, retryAfter)
}

// TestNotesGoLines keeps the file long enough that step 8's cat scrolls a real
// body of raw lines into history rather than a stub.
func TestNotesGoLines(t *testing.T) {
	t.Parallel()
	n := strings.Count(notesGo, "\n")
	assert.GreaterOrEqual(t, n, 170)
}

// TestRetryTestGoKeepsHelper guards step 7's grep target: it must reference
// WithMaxAttempts so a read-only grep finds real content.
func TestRetryTestGoKeepsHelper(t *testing.T) {
	t.Parallel()
	assert.Contains(t, retryTestGo, "WithMaxAttempts")
}

// TestReadmeMDNonEmpty keeps the doc file long enough to read back meaningfully.
func TestReadmeMDNonEmpty(t *testing.T) {
	t.Parallel()
	assert.Greater(t, len(readmeMD), 40)
}

// TestSummaryMarkdown embeds the measured total and mentions the cleanup step.
func TestSummaryMarkdown(t *testing.T) {
	t.Parallel()
	out := summaryMarkdown("12.482")
	assert.Contains(t, out, "total: 12.482s")
	assert.Contains(t, out, "rm -rf")
}

// TestExpandTicks maps @@@ and @@ onto code fences and inline spans.
func TestExpandTicks(t *testing.T) {
	t.Parallel()
	got := expandTicks("a @@b@@ c\n@@@go\nx\n@@@")
	assert.Equal(t, "a `b` c\n```go\nx\n```", got)
}

// TestUnwrap joins soft-wrapped paragraph lines so the terminal reflows them.
func TestUnwrap(t *testing.T) {
	t.Parallel()
	got := unwrap("one two\nthree\n\npara two\ntail")
	assert.Equal(t, "one two three\n\npara two tail\n", got)
}
