package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// multiPick holds one background MultiPickContext call: the UI under test, its
// emulator and input pipe, and the call's result channels.
type multiPick struct {
	u      *UI
	v      *vt
	pw     *io.PipeWriter
	picked chan []int
	err    chan error
}

// startMultiPick runs MultiPickContext over the given items in the background,
// waiting until waitSub renders before returning.
func startMultiPick(t *testing.T, height int, items []PickItem, opts MultiPickOptions, waitSub string) multiPick {
	t.Helper()

	v := newVT(80, height)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	m := multiPick{u: u, v: v, pw: pw, picked: make(chan []int, 1), err: make(chan error, 1)}
	go func() {
		picked, err := u.MultiPickContext(t.Context(), "Tools", items, opts)
		m.err <- err
		m.picked <- picked
	}()
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), waitSub) }, time.Second, testPoll)
	return m
}

// await settles the background call, failing if it never returns.
func (m multiPick) await(t *testing.T) ([]int, error) {
	t.Helper()

	select {
	case picked := <-m.picked:
		return picked, <-m.err
	case <-time.After(time.Second):
		t.Fatal("MultiPick did not settle")
		return nil, nil
	}
}

func TestMultiPick(t *testing.T) {
	t.Parallel()

	t.Run("selects_toggled_rows", func(t *testing.T) {
		m := startMultiPick(t, 12, []PickItem{
			{Label: "read", Group: "builtin"},
			{Label: "ls", Group: "builtin"},
			{Label: "stat", Group: "mcp"},
		}, MultiPickOptions{Placeholder: "filter"}, "Tools")

		// the cursor opens on the builtin header, and down moves to read (row 1)
		press(t, m.pw, "\x1b[B\t") // read
		require.Eventually(t, func() bool { return strings.Contains(m.u.snapshot(m.v), "[x] read") }, time.Second, testPoll)
		// navigate past ls and the mcp header to stat (row 4) and toggle it
		press(t, m.pw, "\x1b[B\x1b[B\x1b[B\t") // down to stat, toggle
		require.Eventually(t, func() bool { return strings.Contains(m.u.snapshot(m.v), "[x] stat") }, time.Second, testPoll)

		press(t, m.pw, "\r") // enter confirms
		picked, err := m.await(t)
		require.NoError(t, err)
		assert.Contains(t, picked, 0) // read
		assert.Contains(t, picked, 2) // stat
		assert.Len(t, picked, 2)
	})

	t.Run("space_toggles_selection", func(t *testing.T) {
		m := startMultiPick(t, 12, []PickItem{{Label: "read"}, {Label: "ls"}}, MultiPickOptions{}, "Tools")

		press(t, m.pw, " ") // no groups here: the cursor is already on read, so space selects it
		require.Eventually(t, func() bool { return strings.Contains(m.u.snapshot(m.v), "[x] read") }, time.Second, testPoll)
		// a typed letter still narrows the filter (space does not become part of it)
		press(t, m.pw, "ls")
		require.Eventually(t, func() bool { return strings.Contains(m.u.snapshot(m.v), "> [ ] ls") }, time.Second, testPoll)

		press(t, m.pw, "\r")
		picked, err := m.await(t)
		require.NoError(t, err)
		assert.Contains(t, picked, 0) // read stayed selected after filtering to ls
	})

	t.Run("group_header_shown", func(t *testing.T) {
		// the divider row costs one live-block line, a tall screen shows both groups
		m := startMultiPick(t, 14, []PickItem{
			{Label: "read", Group: "builtin"},
			{Label: "stat", Group: "mcp"},
		}, MultiPickOptions{}, "builtin")

		assert.Contains(t, m.u.snapshot(m.v), "mcp")
	})

	t.Run("header_toggles_group", func(t *testing.T) {
		m := startMultiPick(t, 12, []PickItem{
			{Label: "read", Group: "builtin"},
			{Label: "ls", Group: "builtin"},
		}, MultiPickOptions{}, "builtin")

		// the cursor starts on the builtin header, and space selects the whole group
		press(t, m.pw, "\t")
		require.Eventually(t, func() bool { return strings.Contains(m.u.snapshot(m.v), "[x] read") }, time.Second, testPoll)

		// toggle again on the header deselects all
		press(t, m.pw, "\t")
		require.Eventually(t, func() bool { return !strings.Contains(m.u.snapshot(m.v), "[x] read") }, time.Second, testPoll)

		// select one member leaves the header partial ([~])
		press(t, m.pw, "\x1b[B\t")
		require.Eventually(t, func() bool { return strings.Contains(m.u.snapshot(m.v), "[~] builtin") }, time.Second, testPoll)

		press(t, m.pw, "\r")
		picked, err := m.await(t)
		require.NoError(t, err)
		assert.Len(t, picked, 1) // only read
	})

	t.Run("headers_never_in_chosen", func(t *testing.T) {
		// the divider row costs one live-block line, a tall screen keeps both groups
		m := startMultiPick(t, 14, []PickItem{
			{Label: "a", Group: "g1"},
			{Label: "b", Group: "g2"},
		}, MultiPickOptions{}, "g1")

		// select both groups via their headers (rows 0 and 2)
		press(t, m.pw, "\t\x1b[B\x1b[B\t")
		require.Eventually(t, func() bool { return strings.Contains(m.u.snapshot(m.v), "[x] b") }, time.Second, testPoll)

		press(t, m.pw, "\r")
		picked, err := m.await(t)
		require.NoError(t, err)
		assert.ElementsMatch(t, []int{0, 1}, picked) // items only, never headers
	})

	t.Run("esc_cancels", func(t *testing.T) {
		m := startMultiPick(t, 12, []PickItem{{Label: "read"}}, MultiPickOptions{}, "Tools")

		press(t, m.pw, "\x1b") // esc
		_, err := m.await(t)
		require.ErrorIs(t, err, ErrCancelled)
	})
}
