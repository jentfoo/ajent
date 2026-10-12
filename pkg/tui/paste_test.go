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
	}, time.Second, testPoll)
	val := u.editorValue()
	assert.Contains(t, val, "pasted")

	// the paste content is stored and expandPastes recovers it whole
	u.mu.Lock()
	expanded := u.expandPastes(val)
	u.mu.Unlock()
	assert.Contains(t, expanded, "line\nline")
	assert.Len(t, expanded, 2500)
}

func TestPastePlaceholdersAreUnique(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	first := strings.Repeat("aaaa\n", 500)
	second := strings.Repeat("bbbb\n", 500)
	go func() {
		_, _ = io.WriteString(pw, "\x1b[200~"+first+"\x1b[201~")
		_, _ = io.WriteString(pw, "\x1b[200~"+second+"\x1b[201~")
	}()

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
	}, time.Second, testPoll)
	assert.NotContains(t, u.editorValue(), "pasted")
}

func TestPasteCRLFStaysOneBreak(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	// Windows terminals deliver CRLF line endings inside a bracketed paste
	go func() {
		_, _ = io.WriteString(pw, "\x1b[200~first\r\nsecond\x1b[201~")
		_ = pw.Close()
	}()

	require.Eventually(t, func() bool {
		return u.editorValue() == "first\nsecond"
	}, time.Second, testPoll)
}

func TestLargePasteStoresNormalizedContent(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	raw := strings.Repeat("line\r\n", 500) // well past pasteThreshold
	go func() {
		_, _ = io.WriteString(pw, "\x1b[200~"+raw+"\x1b[201~")
	}()

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
}
