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
