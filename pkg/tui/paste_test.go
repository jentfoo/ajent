package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPastePlaceholderStoredAndExpanded(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	// a large paste lands as a placeholder in the editor
	large := strings.Repeat("line\n", 500)
	done := make(chan struct{})
	go func() {
		_, _ = io.WriteString(pw, "\x1b[200~"+large+"\x1b[201~")
		close(done)
	}()
	<-done

	require.Eventually(t, func() bool {
		return strings.Contains(u.snapshot(v), "pasted")
	}, time.Second, testPoll, "placeholder must appear")
	val := u.editorValue()
	assert.Contains(t, val, "pasted")

	// the paste content is stored and expandPastes recovers it whole
	u.mu.Lock()
	expanded := u.expandPastes(val)
	u.mu.Unlock()
	assert.Contains(t, expanded, "line\nline")
	assert.Len(t, expanded, 2500, "the placeholder expanded to the full 500-line paste")
}

func TestSmallPasteInsertedInline(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	go func() {
		_, _ = io.WriteString(pw, "\x1b[200~short paste\x1b[201~")
		_ = pw.Close()
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(u.editorValue(), "short paste")
	}, time.Second, testPoll, "a small paste lands inline")
	assert.NotContains(t, u.editorValue(), "pasted")
}
