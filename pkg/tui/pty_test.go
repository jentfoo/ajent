//go:build linux

package tui

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The vt emulator approximates reflow; a real pty is the reference for the
// resize path. The kernel owns the size reports and the line discipline owns
// the bytes, while the slave is not a controlling terminal so no SIGWINCH is
// delivered: the test sets the size and drives the settle the way watchSignals
// would. Keep this rig to the reflow-on-resize cases the emulator can only
// approximate; the emulator carries the broad deterministic coverage.

func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NONBLOCK, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close() })

	var unlock int32 // TIOCSPTLCK: zero unlocks the slave
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
	require.Zero(t, errno, "unlock pty slave")

	var ptyNo int32
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptyNo)))
	require.Zero(t, errno, "pty number")

	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(ptyNo)), os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

// ptyWinsize matches struct winsize from termbits; syscall no longer exports it.
type ptyWinsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

func setPTYSize(t *testing.T, f *os.File, w, h int) {
	t.Helper()
	ws := ptyWinsize{Row: uint16(h), Col: uint16(w)}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	require.Zero(t, errno, "set pty size")
}

// drainPTY reads everything the UI has written so far into the emulator. The
// deadline ends the read; writes are synchronous, so this is deterministic.
func drainPTY(t *testing.T, master *os.File, v *vt) {
	t.Helper()
	require.NoError(t, master.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	buf := make([]byte, 32*1024)
	for {
		n, err := master.Read(buf)
		if n > 0 {
			_, werr := v.Write(buf[:n])
			require.NoError(t, werr)
		}
		if err != nil {
			var timeout bool
			if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) {
				timeout = true
			}
			require.True(t, timeout, "read the pty master: %v", err)
			return
		}
	}
}

// newPTYUI runs a UI in inline mode against a real pty slave, with a fixed
// environment so colour detection does not depend on the test runner.
func newPTYUI(t *testing.T, master, slave *os.File) *UI {
	t.Helper()
	prev := osEnv
	osEnv = func(string) string { return "xterm-256color" }
	t.Cleanup(func() { osEnv = prev })

	u, err := New(Options{In: slave, Out: slave, Mode: ModeInline, Model: "test"})
	require.NoError(t, err)
	t.Cleanup(u.Close)
	return u
}

// TestPTYResizeStrandsNoRow drives narrowing and widening over a real pty and
// asserts what the whole machinery exists to guarantee: exactly one divider,
// committed history never doubled, and the park on the block's top row.
func TestPTYResizeStrandsNoRow(t *testing.T) {
	master, slave := openPTY(t)
	setPTYSize(t, master, 60, 20)
	v := newVT(60, 20)
	u := newPTYUI(t, master, slave)
	drainPTY(t, master, v)

	u.Print("alpha bravo")
	u.Print("charlie delta")
	drainPTY(t, master, v)
	require.Equal(t, 1, countRules(v.Screen()), "one divider before any resize")

	for _, w := range []int{34, 80, 21} {
		setPTYSize(t, master, w, 20)
		v.setSize(w, 20)
		u.holdForResize()
		u.resize()
		drainPTY(t, master, v)

		screen := v.Screen()
		assert.Equal(t, 1, countRules(screen), "width %d: exactly one divider", w)
		assert.Equal(t, 1, strings.Count(screen, "charlie"), "width %d: history never doubled", w)
		// the settled redraw parks on the block's top: the divider row itself
		assert.Equal(t, strings.Repeat(ruleChar, max(w-1, 1)), v.Line(v.row),
			"width %d: parked on the divider row", w)
		assert.Equal(t, 0, v.col, "width %d", w)
	}
}

// TestPTYContaminatedRowParks drives an escape-laden progress row across a
// real resize: the kernel buffer carries the raw bytes, and the sanitize
// funnel must keep the park whole.
func TestPTYContaminatedRowParks(t *testing.T) {
	master, slave := openPTY(t)
	setPTYSize(t, master, 60, 20)
	v := newVT(60, 20)
	u := newPTYUI(t, master, slave)
	drainPTY(t, master, v)

	u.Print("alpha bravo")
	u.SetActivity("call", "write notes.go \x1b[2B 12.4kb\tmore")
	drainPTY(t, master, v)

	setPTYSize(t, master, 30, 20)
	v.setSize(30, 20)
	u.holdForResize()
	u.resize()
	drainPTY(t, master, v)

	screen := v.Screen()
	assert.Equal(t, 1, countRules(screen))
	assert.Contains(t, screen, "write notes.go")
	assert.Equal(t, strings.Repeat(ruleChar, 29), v.Line(v.row), "parked on the divider row")
}
