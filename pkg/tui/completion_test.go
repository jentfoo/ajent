package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCompleter returns a fixed candidate list whatever the buffer holds.
type fakeCompleter struct {
	start int
	items []Completion
}

func (f fakeCompleter) Complete(_ string, _ int) (int, []Completion) {
	return f.start, f.items
}

func TestCompletionTabIsTheOnlyTrigger(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "/model", Label: "/model"},
		{Text: "/memory", Label: "/memory"},
	}})

	// typing offers nothing: no list is raised until Tab asks for one
	press(t, pw, "/m")
	assert.Never(t, func() bool { return strings.Contains(u.snapshot(v), "/memory") }, 100*testPoll, testPoll)

	press(t, pw, "\t") // the candidates agree on "/m" only, so they are listed
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "/memory") }, time.Second, testPoll)
	assert.Contains(t, u.snapshot(v), "/model")
	assert.Equal(t, "/m", u.editorValue())

	press(t, pw, "o") // any key spends the listing
	require.Eventually(t, func() bool { return !strings.Contains(u.snapshot(v), "/memory") }, time.Second, testPoll)
}

func TestCompletionTabCompletesCommonPrefix(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "docs/", Label: "docs/"},
		{Text: "docs2/", Label: "docs2/"},
	}})

	press(t, pw, "doc")
	press(t, pw, "\t") // only the unambiguous portion is filled in
	require.Eventually(t, func() bool { return u.editorValue() == "docs" }, time.Second, testPoll,
		"Tab completes to the common prefix, not the first candidate")

	press(t, pw, "\t") // a second Tab lists rather than guessing
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "docs2/") }, time.Second, testPoll)
	assert.Equal(t, "docs", u.editorValue())
}

func TestCompletionTabAcceptsSingleCandidate(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{{Text: "/model", Label: "/model"}}})

	press(t, pw, "/mo")
	press(t, pw, "\t")
	require.Eventually(t, func() bool { return u.editorValue() == "/model" }, time.Second, testPoll,
		"a lone candidate completes whole")

	// Enter after a completion submits rather than re-applying.
	press(t, pw, "\r")
	select {
	case msg := <-u.Messages():
		assert.Equal(t, "/model", msg)
	case <-time.After(time.Second):
		t.Fatal("Enter after Tab must submit the completed command")
	}
}

// TestCompletionTabTakesTopCandidate covers a source free to return anything: a
// command's own Complete need not extend the typed text, so there is no
// meaningful common prefix and the best candidate is taken whole.
func TestCompletionTabTakesTopCandidate(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "model", Label: "model"},
		{Text: "memory", Label: "memory"},
	}})

	press(t, pw, "/")
	press(t, pw, "\t")
	require.Eventually(t, func() bool { return u.editorValue() == "model" }, time.Second, testPoll)
}

// fakeAtCompleter mimics a path source: it replaces from just past the @ so the
// trigger itself is preserved when a candidate is accepted.
type fakeAtCompleter struct{ items []Completion }

func (f fakeAtCompleter) Complete(text string, pos int) (int, []Completion) {
	if !strings.HasPrefix(text[:min(pos, len(text))], "@") {
		return 0, nil
	}
	return 1, f.items // start past @, like command.pathComplete
}

func TestCompletionTabKeepsAtTrigger(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeAtCompleter{items: []Completion{{Text: "main.go", Label: "main.go"}}})

	press(t, pw, "@")
	press(t, pw, "\t")
	require.Eventually(t, func() bool {
		return u.editorValue() == "@main.go"
	}, time.Second, testPoll, "completing after @ must keep the leading @")
}

// TestCompletionArrowsReachTheEditor proves completion never owns the arrows:
// they stay cursor movement, even with a listing on screen.
func TestCompletionArrowsReachTheEditor(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "docs/", Label: "docs/"},
		{Text: "docs2/", Label: "docs2/"},
	}})

	press(t, pw, "docs")
	press(t, pw, "\t") // ambiguous: a listing appears
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "docs2/") }, time.Second, testPoll)

	press(t, pw, "\x1b[D\x1b[D") // ← ← walks back through the buffer
	require.Eventually(t, func() bool { return u.editorPos() == 2 }, time.Second, testPoll,
		"← must move the caret, not a candidate highlight")

	press(t, pw, "\x1b[A") // ↑ on the first row jumps to the prompt start
	require.Eventually(t, func() bool { return u.editorPos() == 0 }, time.Second, testPoll,
		"↑ must reach the editor rather than a candidate list")
	assert.Equal(t, "docs", u.editorValue())
}

func TestCompletionEscDismissesListing(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "docs/", Label: "docs/"},
		{Text: "docs2/", Label: "docs2/"},
	}})

	press(t, pw, "docs")
	press(t, pw, "\t")
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "docs2/") }, time.Second, testPoll)

	press(t, pw, "\x1b") // Esc drops the listing before it clears the buffer
	require.Eventually(t, func() bool {
		return !strings.Contains(u.snapshot(v), "docs2/")
	}, time.Second, testPoll)
	assert.Equal(t, "docs", u.editorValue())
}

func TestAsyncCompletionKeepsTypingFree(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	// a path source that blocks each query until the test releases it: typing must
	// stay responsive while a listing is pending.
	g := &gatedAsync{queries: make(chan string), answers: make(chan []Completion)}
	u.SetCompleter(g)

	press(t, pw, "@")
	press(t, pw, "\t") // the query starts and blocks
	select {
	case q := <-g.queries:
		assert.Equal(t, "@", q)
	case <-time.After(time.Second):
		t.Fatal("first path query never started")
	}

	// typing while the listing is pending must be accepted immediately
	press(t, pw, "a")
	require.Eventually(t, func() bool { return u.editorValue() == "@a" }, time.Second, testPoll,
		"typing while pending blocked the key loop")

	// the result is for a buffer the user has already moved past: it is dropped
	g.answers <- []Completion{{Text: "stale", Label: "stale"}}
	assert.Never(t, func() bool { return strings.Contains(u.editorValue(), "stale") }, 100*testPoll, testPoll)

	press(t, pw, "\t") // a fresh Tab for the buffer as it stands now
	assert.Equal(t, "@a", <-g.queries)
	g.answers <- []Completion{{Text: "abc.go", Label: "abc.go"}}
	require.Eventually(t, func() bool { return u.editorValue() == "@abc.go" }, time.Second, testPoll)
}

// gatedAsync is an AsyncCompleter whose Complete blocks until the test sends its
// candidates on answers, after reporting the captured text on queries.
type gatedAsync struct {
	queries chan string
	answers chan []Completion
}

func (g *gatedAsync) IsAsync(string, int) bool { return true }
func (g *gatedAsync) Complete(text string, pos int) (int, []Completion) {
	g.queries <- text
	return 1, <-g.answers // start past @; the test drives delivery timing
}

// TestFlashRuleOnSpentTab drives the real key path: a Tab that can neither
// advance the buffer nor show anything new must still light the rule, which
// depends on the keypress repaint rather than one of flashRule's own.
func TestFlashRuleOnSpentTab(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "docs/", Label: "docs/"},
		{Text: "docs2/", Label: "docs2/"},
	}})

	press(t, pw, "docs")
	press(t, pw, "\t") // ambiguous: the listing appears
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "docs2/") }, time.Second, testPoll)
	assert.False(t, u.ruleFlashed())

	press(t, pw, "\t") // nothing left to add and nothing new to show
	require.Eventually(t, u.ruleFlashed, time.Second, testPoll)
	assert.Equal(t, "docs", u.editorValue())
}

// ruleFlashed reads the rule flash state under lock.
func (u *UI) ruleFlashed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.ruleFlash
}

func TestFlashRule(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	fired := make(chan func(), 1)
	u.mu.Lock()
	u.afterDelay = func(_ time.Duration, fn func()) *time.Timer {
		fired <- fn
		return time.NewTimer(time.Hour)
	}
	u.flashRule()
	assert.True(t, u.ruleFlash)
	u.flashRule() // already flashing: no second timer
	u.mu.Unlock()

	clear := <-fired
	assert.Empty(t, fired, "a flash in progress must not arm a second timer")
	clear()
	u.mu.Lock()
	assert.False(t, u.ruleFlash)
	u.mu.Unlock()
}
