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
	require.Eventually(t, func() bool { return u.editorValue() == "docs" }, time.Second, testPoll)

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
	require.Eventually(t, func() bool { return u.editorValue() == "/model" }, time.Second, testPoll)

	// Enter after a completion submits rather than re-applying
	press(t, pw, "\r")
	select {
	case msg := <-u.Messages():
		assert.Equal(t, "/model", msg)
	case <-time.After(time.Second):
		t.Fatal("Enter after Tab must submit the completed command")
	}
}

// TestCompletionTabTakesTopCandidate covers candidates that do not extend the
// typed text, where no common prefix is meaningful.
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

// fakeMenuCompleter is a fixed list presented as a live menu, like a /command.
type fakeMenuCompleter struct{ items []Completion }

func (f fakeMenuCompleter) Complete(_ string, _ int) (int, []Completion) { return 0, f.items }
func (f fakeMenuCompleter) Style(string, int) CompleteStyle              { return CompleteStyle{Menu: true} }

// TestCompletionMenu covers the live presentation /commands and their arguments
// keep: it opens while typing, owns ↑/↓, and accepts on Tab or Enter.
func TestCompletionMenu(t *testing.T) {
	t.Parallel()

	commands := []Completion{
		{Text: "/model", Label: "/model", Detail: "switch model"},
		{Text: "/memory", Label: "/memory"},
	}
	open := func(t *testing.T) (*UI, *vt, *io.PipeWriter) {
		t.Helper()
		v := newVT(80, 12)
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		u := newTestUI(t, v, pr)
		u.SetCompleter(fakeMenuCompleter{items: commands})
		press(t, pw, "/")
		require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "> /model") }, time.Second, testPoll)
		return u, v, pw
	}

	// no Tab needed: the candidates are listed as soon as the context is entered
	t.Run("opens_while_typing", func(t *testing.T) {
		u, v, _ := open(t)
		assert.Contains(t, u.snapshot(v), "/memory")
		assert.Contains(t, u.snapshot(v), "switch model")
	})

	t.Run("arrows_cycle_the_highlight", func(t *testing.T) {
		u, v, pw := open(t)

		press(t, pw, "\x1b[B")
		require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "> /memory") }, time.Second, testPoll)
		press(t, pw, "\x1b[A")
		require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "> /model") }, time.Second, testPoll)
	})

	// Tab takes the highlight whole; a following Enter only submits
	t.Run("tab_accepts_then_enter_submits", func(t *testing.T) {
		u, _, pw := open(t)

		press(t, pw, "\t")
		require.Eventually(t, func() bool { return u.editorValue() == "/model" }, time.Second, testPoll)

		press(t, pw, "\r")
		select {
		case msg := <-u.Messages():
			assert.Equal(t, "/model", msg)
		case <-time.After(time.Second):
			t.Fatal("Enter after Tab must submit the accepted command")
		}
	})

	// Enter on a moved selection accepts it and submits in one press
	t.Run("enter_accepts_moved_selection", func(t *testing.T) {
		u, v, pw := open(t)

		press(t, pw, "\x1b[B")
		require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "> /memory") }, time.Second, testPoll)
		press(t, pw, "\r")
		select {
		case msg := <-u.Messages():
			assert.Equal(t, "/memory", msg)
		case <-time.After(time.Second):
			t.Fatal("Enter must accept the moved selection and submit")
		}
	})

	t.Run("esc_dismisses", func(t *testing.T) {
		u, v, pw := open(t)

		press(t, pw, "\x1b")
		require.Eventually(t, func() bool { return !strings.Contains(u.snapshot(v), "/memory") }, time.Second, testPoll)
		assert.Equal(t, "/", u.editorValue())
	})
}

// fakeAtCompleter mimics a path source, replacing from just past the @.
type fakeAtCompleter struct{ items []Completion }

func (f fakeAtCompleter) Complete(text string, pos int) (int, []Completion) {
	if !strings.HasPrefix(text[:min(pos, len(text))], "@") {
		return 0, nil
	}
	return 1, f.items // start past @, like command.pathComplete
}

// TestCompletionAtReferences pins @ to the same rules as every other context.
// Its candidates replace from past the trigger, so every outcome is covered
// again on that shape.
func TestCompletionAtReferences(t *testing.T) {
	t.Parallel()

	files := []Completion{
		{Text: "main.go", Label: "main.go"},
		{Text: "main_test.go", Label: "main_test.go"},
	}

	// a lone candidate fills in whole, keeping the trigger
	t.Run("fills_keeping_trigger", func(t *testing.T) {
		v := newVT(80, 12)
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		u := newTestUI(t, v, pr)
		u.SetCompleter(fakeAtCompleter{items: files[:1]})

		press(t, pw, "@")
		press(t, pw, "\t")
		require.Eventually(t, func() bool { return u.editorValue() == "@main.go" }, time.Second, testPoll)
	})

	// ambiguity fills only the shared prefix, then lists on the next Tab
	t.Run("stops_at_ambiguity", func(t *testing.T) {
		v := newVT(80, 12)
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		u := newTestUI(t, v, pr)
		u.SetCompleter(fakeAtCompleter{items: files})

		press(t, pw, "@mai")
		press(t, pw, "\t")
		require.Eventually(t, func() bool { return u.editorValue() == "@main" }, time.Second, testPoll)
		assert.NotContains(t, u.snapshot(v), "main_test.go")

		press(t, pw, "\t")
		require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "main_test.go") }, time.Second, testPoll)
		assert.Equal(t, "@main", u.editorValue())
	})

	// the arrows stay the editor's even with an @ listing on screen
	t.Run("arrows_reach_the_editor", func(t *testing.T) {
		v := newVT(80, 12)
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		u := newTestUI(t, v, pr)
		u.SetCompleter(fakeAtCompleter{items: files})

		press(t, pw, "@main")
		press(t, pw, "\t") // ambiguous: a listing appears
		require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "main_test.go") }, time.Second, testPoll)

		press(t, pw, "\x1b[D\x1b[D") // ← ← walks back through the buffer
		require.Eventually(t, func() bool { return u.editorPos() == 3 }, time.Second, testPoll)
		assert.Equal(t, "@main", u.editorValue())
	})
}

// TestCompletionArrowsReachTheEditor proves the arrows stay cursor movement,
// even with a listing on screen.
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
	require.Eventually(t, func() bool { return u.editorPos() == 2 }, time.Second, testPoll)

	press(t, pw, "\x1b[A") // ↑ on the first row jumps to the prompt start
	require.Eventually(t, func() bool { return u.editorPos() == 0 }, time.Second, testPoll)
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

	// a source blocking until the test releases it; typing must stay responsive
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
	require.Eventually(t, func() bool { return u.editorValue() == "@a" }, time.Second, testPoll)

	// the buffer has moved past this result, so it is dropped
	g.answers <- []Completion{{Text: "stale", Label: "stale"}}
	assert.Never(t, func() bool { return strings.Contains(u.editorValue(), "stale") }, 100*testPoll, testPoll)

	press(t, pw, "\t") // a fresh Tab for the buffer as it stands now
	assert.Equal(t, "@a", <-g.queries)
	g.answers <- []Completion{{Text: "abc.go", Label: "abc.go"}}
	require.Eventually(t, func() bool { return u.editorValue() == "@abc.go" }, time.Second, testPoll)
}

// gatedAsync reports each query's text on queries, then blocks until the test
// sends its candidates on answers.
type gatedAsync struct {
	queries chan string
	answers chan []Completion
}

func (g *gatedAsync) Style(string, int) CompleteStyle { return CompleteStyle{Async: true} }
func (g *gatedAsync) Complete(text string, pos int) (int, []Completion) {
	g.queries <- text
	return 1, <-g.answers // start past @; the test drives delivery timing
}

// TestFlashRuleOnSpentTab drives the real key path, where the flash depends on
// the keypress repaint rather than one of flashRule's own.
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
	assert.Empty(t, fired)
	clear()
	u.mu.Lock()
	assert.False(t, u.ruleFlash)
	u.mu.Unlock()
}
