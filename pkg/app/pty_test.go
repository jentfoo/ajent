//go:build linux

package app

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTTY returns one end of a fresh pty, a real terminal for isTerminal. The
// master stays open until cleanup or reads on the slave would fail.
func openTTY(t *testing.T) *os.File {
	t.Helper()

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NONBLOCK, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close() })

	var unlock int32 // TIOCSPTLCK: zero unlocks the slave
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
	require.Zero(t, errno)

	var ptyNo int32
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptyNo)))
	require.Zero(t, errno)

	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(ptyNo)), os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close() })
	return slave
}

func TestRouteStartup(t *testing.T) {
	t.Parallel()

	// piped returns stdin carrying payload with the writer closed, so a read
	// drains at once and lands on EOF
	piped := func(payload string) func(*testing.T) *os.File {
		return func(t *testing.T) *os.File {
			t.Helper()
			r, w, err := os.Pipe()
			require.NoError(t, err)
			if payload != "" {
				_, err = w.WriteString(payload)
				require.NoError(t, err)
			}
			require.NoError(t, w.Close())
			t.Cleanup(func() { _ = r.Close() })
			return r
		}
	}
	// broken returns a stream every read fails on, standing in for stdin errors
	broken := func(t *testing.T) *os.File {
		t.Helper()
		r, w, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
		require.NoError(t, r.Close())
		return r
	}

	tests := []struct {
		name   string
		prompt string // the -p value
		args   []string
		stdin  func(*testing.T) *os.File
		stdout func(*testing.T) *os.File
		want   startupRoute
		text   string // headless prompt, or usage message; a prefix for read errors
	}{
		{
			name:   "prompt flag wins",
			prompt: "explain the flake",
			stdin:  broken, // -p must never drain stdin
			stdout: piped(""),
			want:   routeHeadless,
			text:   "explain the flake",
		},
		{
			name:   "flag prompt trimmed",
			prompt: "  explain the flake  ",
			stdin:  broken, // -p must never drain stdin
			stdout: piped(""),
			want:   routeHeadless,
			text:   "explain the flake",
		},
		{
			name:   "argv args win", // the argv read must never decide the route first
			args:   []string{"why did this fail"},
			stdin:  broken, // an explicit prompt beside stdin is never drained either
			stdout: piped(""),
			want:   routeHeadless,
			text:   "why did this fail",
		},
		{
			name:   "argv args joined and trimmed",
			args:   []string{"  why did this", "fail"},
			stdin:  broken,
			stdout: piped(""),
			want:   routeHeadless,
			text:   "why did this fail",
		},
		{
			name:   "argv args beat blank pipe", // the seeded prompt 3874865 routed away
			args:   []string{"why did this fail"},
			stdin:  piped("\n"),
			stdout: openTTY,
			want:   routeHeadless,
			text:   "why did this fail",
		},
		{
			name:   "blank argv falls to streams",
			args:   []string{"  ", ""},
			stdin:  piped(""),
			stdout: openTTY,
			want:   routeUsage,
			text:   "piped stdin was empty; use --prompt or pipe a prompt on stdin",
		},
		{
			name:   "argv args in terminal pair",
			args:   []string{"seed question"},
			stdin:  openTTY,
			stdout: openTTY,
			want:   routeInteractive,
		},
		{
			name:   "piped payload trimmed",
			prompt: "",
			stdin:  piped("\n  hello world \r\n"),
			stdout: piped(""),
			want:   routeHeadless,
			text:   "hello world",
		},
		{
			name:   "empty pipe tty stdout",
			prompt: "",
			stdin:  piped(""),
			stdout: openTTY, // the fall-through this test pins to a usage error
			want:   routeUsage,
			text:   "piped stdin was empty; use --prompt or pipe a prompt on stdin",
		},
		{
			name:   "empty pipe redirected stdout",
			prompt: "",
			stdin:  piped(""),
			stdout: piped(""),
			want:   routeUsage,
			text:   "piped stdin was empty; use --prompt or pipe a prompt on stdin",
		},
		{
			name:   "terminal stdin redirected stdout",
			prompt: "",
			stdin:  openTTY,
			stdout: piped(""),
			want:   routeUsage,
			text:   "stdout is not a terminal; use --prompt or pipe a prompt on stdin",
		},
		{
			name:   "terminal pair interactive",
			prompt: "",
			stdin:  openTTY,
			stdout: openTTY,
			want:   routeInteractive,
		},
		{
			name:   "stdin read error",
			prompt: "",
			stdin:  broken,
			stdout: piped(""),
			want:   routeUsage,
			text:   "reading piped stdin: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, text := routeStartup(tt.prompt, tt.args, tt.stdin(t), tt.stdout(t))
			assert.Equal(t, tt.want, route)
			switch route {
			case routeHeadless:
				assert.Equal(t, tt.text, text)
			case routeUsage:
				assert.True(t, strings.HasPrefix(text, tt.text))
			default:
				assert.Empty(t, text) // interactive carries nothing back
			}
		})
	}
}
