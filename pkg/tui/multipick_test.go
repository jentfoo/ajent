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

	// toggle the first row (read) and the third (stat, in the mcp group)
	press(t, pw, "\t") // read
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "[x] read") }, time.Second, testPoll)
	press(t, pw, "\x1b[B\x1b[B") // down, down to stat
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "> [ ] stat") }, time.Second, testPoll)
	press(t, pw, "\t") // toggle stat
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

	press(t, pw, " ") // space toggles the highlighted row (read) on
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

	v := newVT(80, 12)
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
