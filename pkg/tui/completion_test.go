package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCompleter returns a fixed candidate list for a given prefix.
type fakeCompleter struct {
	start int
	items []Completion
}

func (f fakeCompleter) Complete(_ string, _ int) (int, []Completion) {
	return f.start, f.items
}

func TestSetCompleterEnablesOverlay(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "model", Label: "/model"},
		{Text: "memory", Label: "/memory"},
	}})

	// type / to trigger the query
	press(t, pw, "/")
	require.Eventually(t, func() bool {
		return strings.Contains(u.snapshot(v), "/model")
	}, time.Second, testPoll, "overlay must show candidates after /")
	assert.Contains(t, u.snapshot(v), "/memory")
}

func TestCompletionTabAcceptsAndInserts(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "model", Label: "/model"},
	}})

	press(t, pw, "/")
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "/model") }, time.Second, testPoll)

	press(t, pw, "\t") // accept
	require.Eventually(t, func() bool {
		return strings.Contains(u.editorValue(), "model")
	}, time.Second, testPoll, "Tab accepts the candidate into the editor")
}

func TestCompletionTabThenEnterSubmits(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "model", Label: "/model"},
	}})

	press(t, pw, "/")
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "/model") }, time.Second, testPoll)
	press(t, pw, "\t") // accept via Tab
	require.Eventually(t, func() bool { return u.editorValue() == "model" }, time.Second, testPoll,
		"Tab inserts the candidate")

	// Enter after a Tab-accepted selection submits rather than re-applying.
	press(t, pw, "\r")
	select {
	case msg := <-u.Messages():
		assert.Equal(t, "model", msg)
	case <-time.After(time.Second):
		t.Fatal("Enter after Tab must submit the accepted command")
	}
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
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "main.go") }, time.Second, testPoll)

	press(t, pw, "\t") // accept the path candidate
	require.Eventually(t, func() bool {
		return u.editorValue() == "@main.go"
	}, time.Second, testPoll, "accepting a @ completion must keep the leading @")
}

func TestAsyncPathKeepsTypingFreeAndLatestWins(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	// a path source that blocks each query until the test releases it: typing must
	// stay responsive while results are pending.
	g := &gatedAsync{queries: make(chan string), answers: make(chan []Completion)}
	u.SetCompleter(g)

	press(t, pw, "@") // first query starts and blocks
	select {
	case q := <-g.queries:
		assert.Equal(t, "@", q)
	case <-time.After(time.Second):
		t.Fatal("first path query never started")
	}

	// typing while the listing is pending must be accepted immediately (the key
	// loop is not held up), starting a second, superseding query.
	press(t, pw, "a")
	select {
	case q := <-g.queries:
		assert.Equal(t, "@a", q)
	case <-time.After(time.Second):
		t.Fatal("typing while pending blocked the key loop; second query never started")
	}

	// a stale result for the older generation is dropped: nothing shows
	g.answers <- []Completion{{Text: "stale", Label: "stale"}}
	require.Eventually(t, func() bool {
		return !strings.Contains(u.snapshot(v), "stale")
	}, time.Second, testPoll)

	// the newest generation's result replaces it.
	g.answers <- []Completion{{Text: "fresh", Label: "fresh"}}
	require.Eventually(t, func() bool {
		return strings.Contains(u.snapshot(v), "fresh")
	}, time.Second, testPoll)
}

// gatedAsync is an AsyncPathCompleter whose Complete blocks until the test sends
// its candidates on answers, after reporting the captured text on queries.
type gatedAsync struct {
	queries chan string
	answers chan []Completion
}

func (g *gatedAsync) IsAsyncPath(string, int) bool { return true }
func (g *gatedAsync) Complete(text string, pos int) (int, []Completion) {
	g.queries <- text
	return 1, <-g.answers // start past @; the test drives delivery timing
}

// gatedFlip is async only while the cursor sits in an @ token; outside it behaves
// like a synchronous empty completer, mirroring command completion.
type gatedFlip struct {
	*gatedAsync
}

func (g *gatedFlip) IsAsyncPath(text string, pos int) bool { return strings.HasPrefix(text, "@") }
func (g *gatedFlip) Complete(text string, pos int) (int, []Completion) {
	if !strings.HasPrefix(text, "@") {
		return 0, nil
	}
	g.queries <- text
	return 1, <-g.answers
}

// TestStalePathDroppedAfterLeaving ensures a slow @ listing that returns after the
// user has typed out of the path token is discarded: synchronous completion bumps
// the generation so its pending result cannot reopen an overlay for dead text.
func TestStalePathDroppedAfterLeaving(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	g := &gatedFlip{gatedAsync: &gatedAsync{queries: make(chan string), answers: make(chan []Completion)}}
	u.SetCompleter(g)
	press(t, pw, "@")
	assert.Equal(t, "@", <-g.queries) // a path listing starts and blocks

	// backspace out of the @ region before it returns; completion now runs
	// synchronously (and finds nothing), so no overlay should ever open.
	press(t, pw, "\x7f")

	g.answers <- []Completion{{Text: "stale", Label: "stale"}}
	assert.Never(t, func() bool {
		return strings.Contains(u.snapshot(v), "stale")
	}, 200*testPoll, testPoll)
}

func TestCompletionEscDismisses(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "model", Label: "/model"},
	}})

	press(t, pw, "/")
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "/model") }, time.Second, testPoll)

	press(t, pw, "\x1b") // Esc dismisses the overlay
	require.Eventually(t, func() bool {
		return !strings.Contains(u.snapshot(v), "/model")
	}, time.Second, testPoll, "Esc closes the overlay")
}

func TestCompletionUpDownCycles(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)
	u.SetCompleter(fakeCompleter{start: 0, items: []Completion{
		{Text: "model", Label: "/model"},
		{Text: "memory", Label: "/memory"},
	}})

	press(t, pw, "/")
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "> /model") }, time.Second, testPoll)

	press(t, pw, "\x1b[B") // down
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "> /memory") }, time.Second, testPoll,
		"down moves the cursor to /memory")
	press(t, pw, "\x1b[A") // up
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "> /model") }, time.Second, testPoll,
		"up moves back to /model")
}
