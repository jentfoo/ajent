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
// terminal lifecycle. The kernel owns the size reports and the line discipline
// owns the bytes, so raw mode, keystroke transport, size and teardown are all
// real here. Only the origin of SIGWINCH is not: the slave is not a controlling
// terminal, so the kernel delivers nothing and the test raises the signal on
// itself, which drives the whole real watchSignals chain. Keep this rig to the
// cases the emulator cannot reach; the emulator carries the broad coverage.
//
// None of these tests may call t.Parallel: signal.Notify is process wide, so two
// live UIs would answer each other's SIGWINCH. Only a UI built through New runs
// watchSignals, and every such test lives in this file.

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

// pumpPTY moves everything readable on the master into the emulator, if one is
// given, and returns the raw bytes. Read failures come back as an error rather
// than a test failure so it can run inside a poll loop.
func pumpPTY(master *os.File, v *vt, wait time.Duration) (string, error) {
	if err := master.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return "", err
	}
	var raw strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := master.Read(buf)
		if n > 0 {
			raw.Write(buf[:n])
			if v != nil {
				if _, werr := v.Write(buf[:n]); werr != nil {
					return raw.String(), werr
				}
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) {
				return raw.String(), nil
			}
			return raw.String(), err
		}
	}
}

// drainPTY reads everything the UI has written so far into the emulator. The
// deadline ends the read; writes are synchronous, so this is deterministic.
func drainPTY(t *testing.T, master *os.File, v *vt) {
	t.Helper()
	_, err := pumpPTY(master, v, 100*time.Millisecond)
	require.NoError(t, err, "read the pty master")
}

// readPTY returns the raw bytes waiting on the master, for asserting on the
// escape sequences themselves rather than their effect.
func readPTY(t *testing.T, master *os.File) string {
	t.Helper()
	raw, err := pumpPTY(master, nil, 100*time.Millisecond)
	require.NoError(t, err, "read the pty master")
	return raw
}

// sendPTY types bytes into the master and drains what the UI writes back.
func sendPTY(t *testing.T, master *os.File, v *vt, s string) {
	t.Helper()
	_, err := master.WriteString(s)
	require.NoError(t, err)
	drainPTY(t, master, v)
}

// eventuallyPTY pumps the master into the emulator until cond holds, standing in
// for a sleep against the real settle timers. The pump's own read deadline
// paces the loop, and everything runs on the test goroutine so the emulator is
// never read and written at once.
func eventuallyPTY(t *testing.T, master *os.File, v *vt, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := pumpPTY(master, v, 2*time.Millisecond)
		require.NoError(t, err, "read the pty master")
		if cond() {
			return
		}
	}
	t.Fatalf("pty never settled: %s\n%s", msg, v.Screen())
}

// termiosOf reads a terminal's line discipline, for raw-mode assertions.
func termiosOf(t *testing.T, f *os.File) syscall.Termios {
	t.Helper()
	var tio syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&tio)))
	require.Zero(t, errno, "read termios")
	return tio
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

// TestPTYKeystrokes types through the kernel's line discipline: the bytes reach
// the decoder, the line submits, and raw mode means the terminal never echoed
// them, so the only copy on screen is the one the UI painted.
func TestPTYKeystrokes(t *testing.T) {
	master, slave := openPTY(t)
	setPTYSize(t, master, 60, 20)
	v := newVT(60, 20)
	u := newPTYUI(t, master, slave)
	drainPTY(t, master, v)

	sendPTY(t, master, v, "hello")
	eventuallyPTY(t, master, v, func() bool {
		return strings.Contains(v.Screen(), "hello")
	}, "typed text reaches the editor row")
	assert.Equal(t, 1, strings.Count(v.Screen(), "hello"), "ECHO is off, so nothing echoed it back")

	sendPTY(t, master, v, "\r")
	select {
	case got := <-u.Messages():
		assert.Equal(t, "hello", got)
	case <-time.After(5 * time.Second):
		t.Fatal("Enter never submitted the typed line")
	}
}

// TestPTYRawMode asserts the line discipline itself, the state the whole
// renderer assumes: raw while the UI is live, exactly as found on teardown.
func TestPTYRawMode(t *testing.T) {
	master, slave := openPTY(t)
	setPTYSize(t, master, 60, 20)
	v := newVT(60, 20)

	cooked := termiosOf(t, slave)
	require.NotZero(t, cooked.Lflag&syscall.ECHO, "the pty starts cooked")

	u := newPTYUI(t, master, slave)
	drainPTY(t, master, v)

	live := termiosOf(t, slave)
	assert.Zero(t, live.Lflag&syscall.ECHO)
	assert.Zero(t, live.Lflag&syscall.ICANON)

	u.Close()
	assert.Equal(t, cooked.Lflag, termiosOf(t, slave).Lflag, "Close restores what it found")
}

// TestPTYSignalResize drives the real signal path end to end: watchSignals
// debounces the burst, probeResize writes the DSR barrier query, the answer
// releases the settled redraw. Nothing here is simulated but the signal's
// sender.
func TestPTYSignalResize(t *testing.T) {
	master, slave := openPTY(t)
	setPTYSize(t, master, 60, 20)
	v := newVT(60, 20)
	u := newPTYUI(t, master, slave)
	drainPTY(t, master, v)

	u.Print("alpha bravo")
	u.Print("charlie delta")
	drainPTY(t, master, v)
	require.Equal(t, 1, countRules(v.Screen()), "one divider before any resize")

	probes := v.dsrCount
	setPTYSize(t, master, 34, 20)
	v.setSize(34, 20)
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGWINCH))

	eventuallyPTY(t, master, v, func() bool {
		return v.dsrCount > probes
	}, "the settled burst raises the barrier query")

	sendPTY(t, master, v, "\x1b[0n")
	eventuallyPTY(t, master, v, func() bool {
		return v.Line(v.row) == strings.Repeat(ruleChar, 33)
	}, "the answered barrier parks on the divider row")

	screen := v.Screen()
	assert.Equal(t, 1, countRules(screen))
	assert.Equal(t, 1, strings.Count(screen, "charlie"), "history never doubled")
	assert.Equal(t, 0, v.col)
	assert.Equal(t, 34, u.Width(), "the renderer read the kernel's new size")
}

// TestPTYSignalResizeReanchors drives the re-anchor over a real pty. The pty
// carries no terminal of its own, so the test writes the replies a clamped
// terminal would send.
func TestPTYSignalResizeReanchors(t *testing.T) {
	master, slave := openPTY(t)
	setPTYSize(t, master, 80, 24)
	v := newVT(80, 24)
	u := newPTYUI(t, master, slave)
	drainPTY(t, master, v)

	words := testWords
	u.Print(strings.Repeat(words, 4))
	u.Text(strings.Repeat(words, 8)) // an unclosed block
	drainPTY(t, master, v)
	require.Equal(t, 1, countRules(v.Screen()))

	setPTYSize(t, master, 48, 24)
	v.setSize(48, 24)
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGWINCH))

	eventuallyPTY(t, master, v, func() bool {
		return v.cprCount > 0
	}, "the settled burst raises the cursor query")

	// the reflow overflowed the screen and clamped the park onto the top row
	_, err := master.WriteString("\x1b[1;1R\x1b[0n")
	require.NoError(t, err)
	eventuallyPTY(t, master, v, func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return v.row == v.h-len(u.render.(*inlineRenderer).live) && v.col == 0
	}, "the re-anchored block sits at the screen bottom")

	assert.Equal(t, 1, countRules(v.Screen()))
	assert.Contains(t, v.Line(v.h-1), "test")
	assert.Equal(t, 48, u.Width())
}

// TestPTYTeardown asserts what a terminal is left holding after Close: the
// cursor back, bracketed paste off, and a second Close changing nothing.
func TestPTYTeardown(t *testing.T) {
	master, slave := openPTY(t)
	setPTYSize(t, master, 60, 20)
	v := newVT(60, 20)
	u := newPTYUI(t, master, slave)
	u.Print("alpha bravo")
	drainPTY(t, master, v) // everything up to here is startup and paint

	u.Close()
	restore := readPTY(t, master)
	assert.Contains(t, restore, showCursor)
	assert.Contains(t, restore, bracketedPasteOff)

	u.Close()
	assert.Empty(t, readPTY(t, master), "the second Close writes nothing")
}
