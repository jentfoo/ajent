// Package clipboard writes text to the system clipboard for every copy
// surface (/copy and the rewind picker's ctrl+x). One writer, one contract:
// platform-native writers first, OSC 52 on remote sessions, and success only
// when a write verifiably landed.
package clipboard

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// writeTimeout bounds one native writer; a hung backend degrades to trying the
// next one instead of stalling the copy.
const writeTimeout = 3 * time.Second

// maxEncoded caps the base64 payload OSC 52 will carry, so an oversized copy
// refuses outright instead of truncating partway through the escape sequence.
const maxEncoded = 100_000

// writer is one platform clipboard writer tried in order.
type writer struct {
	name string   // executable looked up on PATH
	args []string // arguments making it read clipboard text from stdin
	env  []string // display variables it needs; any one set suffices
}

// writers lists the native writers for the running platform, best first. The
// platform's own tool leads so the terminal cannot race the write; clip.exe
// closes the Linux list for WSL interop. A var so tests inject fakes.
var writers = func() []writer {
	switch runtime.GOOS {
	case "darwin":
		return []writer{
			{"pbcopy", nil, nil},
		}
	case "windows":
		// Set-Clipboard is the native API; clip is the slim fallback
		return []writer{
			{"powershell.exe", []string{"-NoProfile", "-Command", "$input | Set-Clipboard"}, nil},
			{"clip", nil, nil},
		}
	default:
		return []writer{
			{"termux-clipboard-set", nil, nil},
			{"wl-copy", nil, []string{"WAYLAND_DISPLAY", "WAYLAND_SOCKET"}},
			{"xclip", []string{"-selection", "clipboard"}, []string{"DISPLAY"}},
			{"xsel", []string{"--clipboard", "--input"}, []string{"DISPLAY"}},
			{"clip.exe", nil, nil}, // WSL interop
		}
	}
}

// envValue is the environment lookup behind the display gates. A var so tests
// fake the environment.
var envValue = os.Getenv

// envSet reports whether a writer's display requirements are met: no
// requirements, or any listed variable set.
func envSet(names []string) bool {
	if len(names) == 0 {
		return true
	}
	for _, n := range names {
		if envValue(n) != "" {
			return true
		}
	}
	return false
}

// available reports whether a writer's executable is installed. A var so tests
// fake the lookup.
var available = func(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// runWriter feeds text to one native writer. A var so tests fake the subprocess.
var runWriter = func(ctx context.Context, w writer, text string) error {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	cmd := exec.CommandContext(wctx, w.name, w.args...)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stderr = io.Discard // backend noise never reaches the tty
	return cmd.Run()
}

// remoteEnvVars name the session kinds whose terminal sits between us and the
// user's machine.
var remoteEnvVars = []string{"SSH_CONNECTION", "SSH_CLIENT", "MOSH_CONNECTION"}

// remote reports a remote session, where the terminal is the only clipboard OSC
// 52 can still reach. A var so tests fake the environment.
var remote = func() bool {
	for _, k := range remoteEnvVars {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// stdout receives the OSC 52 fallback. A var so tests capture the sequence.
var stdout io.Writer = os.Stdout

// Copy writes text to the system clipboard, returning an error with install
// guidance on total failure. Success means a native writer verifiably
// accepted the payload, or on a remote session that the OSC 52 fallback was
// emitted. There OSC 52 also accompanies a native success, since a forwarded
// display can accept a write that never reaches the user's machine.
func Copy(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var tried, displayless []string
	for _, w := range writers() {
		if !available(w.name) {
			continue
		}
		if !envSet(w.env) {
			displayless = append(displayless, w.name) // installed, no display to reach
			continue
		}
		tried = append(tried, w.name)
		if runWriter(ctx, w, text) == nil {
			if remote() {
				_ = writeOSC52(text) // best effort; the native write already landed
			}
			return nil
		}
	}
	if remote() {
		return writeOSC52(text)
	}
	return nativeError(tried, displayless)
}

// nativeError explains total failure in platform terms.
func nativeError(tried, displayless []string) error {
	if len(tried) > 0 {
		return fmt.Errorf("clipboard write failed (tried %s)", strings.Join(tried, ", "))
	}
	if len(displayless) > 0 {
		return fmt.Errorf("%s need a display session (WAYLAND_DISPLAY or DISPLAY)",
			strings.Join(displayless, ", "))
	}
	switch runtime.GOOS {
	case "darwin":
		return errors.New("clipboard unavailable: pbcopy not found")
	case "windows":
		return errors.New("clipboard unavailable: clip and powershell not found")
	default:
		return errors.New("no clipboard writer on PATH; install xclip, xsel or wl-copy to enable copying")
	}
}

// writeOSC52 emits the terminal clipboard escape, refusing a payload past the
// encoded limit rather than truncating it.
func writeOSC52(text string) error {
	enc := base64.StdEncoding.EncodeToString([]byte(text))
	if len(enc) > maxEncoded {
		return fmt.Errorf("clipboard payload exceeds the %dkb OSC 52 limit; copy less text", maxEncoded/1000)
	}
	if _, err := io.WriteString(stdout, "\x1b]52;c;"+enc+"\x07"); err != nil {
		return fmt.Errorf("OSC 52 clipboard write failed: %w", err)
	}
	return nil
}
