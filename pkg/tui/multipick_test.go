package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMultiPickSelects(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	items := []PickItem{
		{Label: "read", Group: "builtin"},
		{Label: "ls", Group: "builtin"},
		{Label: "stat", Group: "mcp"},
	}

	result := make(chan []int, 1)
	errCh := make(chan error, 1)
	go func() {
		picked, err := u.MultiPick("Tools", items, MultiPickOptions{Placeholder: "filter"})
		errCh <- err
		result <- picked
	}()

	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "Tools") }, time.Second, testPoll)

	// the cursor opens on the builtin header; down moves to read (row 1)
	press(t, pw, "\x1b[B\t") // read
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "[x] read") }, time.Second, testPoll)
	// navigate past ls and the mcp header to stat (row 4) and toggle it
	press(t, pw, "\x1b[B\x1b[B\x1b[B\t") // down to stat, toggle
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "[x] stat") }, time.Second, testPoll)

	press(t, pw, "\r") // enter confirms
	select {
	case picked := <-result:
		require.NoError(t, <-errCh)
		assert.Contains(t, picked, 0) // read
		assert.Contains(t, picked, 2) // stat
		assert.Len(t, picked, 2)
	case <-time.After(time.Second):
		t.Fatal("MultiPick did not return")
	}
}

func TestMultiPickSpaceTogglesSelection(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	items := []PickItem{{Label: "read"}, {Label: "ls"}}
	result := make(chan []int, 1)
	errCh := make(chan error, 1)
	go func() {
		picked, err := u.MultiPick("Tools", items, MultiPickOptions{})
		errCh <- err
		result <- picked
	}()
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "Tools") }, time.Second, testPoll)

	press(t, pw, " ") // no groups here: the cursor is already on read; space selects it
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "[x] read") }, time.Second, testPoll,
		"space selects the highlighted tool without entering the filter")
	// a typed letter still narrows the filter (space does not become part of it)
	press(t, pw, "ls")
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "> [ ] ls") }, time.Second, testPoll,
		"typing narrows the list and space never polluted the filter")

	press(t, pw, "\r")
	select {
	case picked := <-result:
		require.NoError(t, <-errCh)
		assert.Contains(t, picked, 0) // read stayed selected after filtering to ls
	case <-time.After(time.Second):
		t.Fatal("MultiPick did not return")
	}
}

func TestMultiPickGroupHeaderShown(t *testing.T) {
	t.Parallel()

	// the divider row costs one live-block line; a taller screen shows both groups.
	v := newVT(80, 14)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	items := []PickItem{
		{Label: "read", Group: "builtin"},
		{Label: "stat", Group: "mcp"},
	}

	go func() { _, _ = u.MultiPick("Tools", items, MultiPickOptions{}) }()

	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "builtin") }, time.Second, testPoll)
	assert.Contains(t, u.snapshot(v), "mcp")
}

func TestMultiPickHeaderTogglesGroup(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	items := []PickItem{
		{Label: "read", Group: "builtin"},
		{Label: "ls", Group: "builtin"},
	}
	result := make(chan []int, 1)
	errCh := make(chan error, 1)
	go func() {
		picked, err := u.MultiPick("Tools", items, MultiPickOptions{})
		errCh <- err
		result <- picked
	}()

	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "builtin") }, time.Second, testPoll)
	// the cursor starts on the builtin header; space selects the whole group
	press(t, pw, "\t")
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "[x] read") }, time.Second, testPoll,
		"toggling a header selects every member of its group")

	// toggle again on the header deselects all
	press(t, pw, "\t")
	require.Eventually(t, func() bool { return !strings.Contains(u.snapshot(v), "[x] read") }, time.Second, testPoll,
		"a fully selected group toggles back off as a whole")

	// select one member leaves the header partial ([~])
	press(t, pw, "\x1b[B\t")
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "[~] builtin") }, time.Second, testPoll,
		"a partially selected group renders a tri-state checkbox")

	press(t, pw, "\r")
	select {
	case picked := <-result:
		require.NoError(t, <-errCh)
		assert.Len(t, picked, 1) // only read
	case <-time.After(time.Second):
		t.Fatal("MultiPick did not return")
	}
}

func TestMultiPickHeaderNeverInChosen(t *testing.T) {
	t.Parallel()

	// the divider row costs one live-block line; a taller screen keeps both groups.
	v := newVT(80, 14)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	items := []PickItem{
		{Label: "a", Group: "g1"},
		{Label: "b", Group: "g2"},
	}
	result := make(chan []int, 1)
	errCh := make(chan error, 1)
	go func() {
		picked, err := u.MultiPick("Tools", items, MultiPickOptions{})
		errCh <- err
		result <- picked
	}()
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "g1") }, time.Second, testPoll)

	// select both groups via their headers (rows 0 and 2)
	press(t, pw, "\t\x1b[B\x1b[B\t")
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "[x] b") }, time.Second, testPoll)

	press(t, pw, "\r")
	select {
	case picked := <-result:
		require.NoError(t, <-errCh)
		assert.ElementsMatch(t, []int{0, 1}, picked) // items only, never headers
	case <-time.After(time.Second):
		t.Fatal("MultiPick did not return")
	}
}

func TestMultiPickEscCancels(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	errCh := make(chan error, 1)
	go func() {
		_, err := u.MultiPick("Tools", []PickItem{{Label: "read"}}, MultiPickOptions{})
		errCh <- err
	}()

	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "Tools") }, time.Second, testPoll)
	press(t, pw, "\x1b") // esc
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, ErrCancelled)
	case <-time.After(time.Second):
		t.Fatal("MultiPick did not cancel")
	}
}
