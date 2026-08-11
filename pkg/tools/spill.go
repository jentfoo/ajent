package tools

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// spiller lazily creates a per-session spill file in os.TempDir for bash output
// that exceeds the in-memory budget, so a normal command leaves nothing behind.
type spiller struct {
	sessionID string
	f         *os.File
	path      string
}

// newSpiller returns a spiller writing into a session-named temp directory. It
// is not created until Write first needs it.
func newSpiller(sessionID string) *spiller { return &spiller{sessionID: sessionID} }

// Write appends p to the spill file, creating both lazily on first use.
func (s *spiller) Write(p []byte) (int, error) {
	if s.f == nil {
		f, path, err := createSpill(s.sessionID)
		if err != nil {
			return 0, err
		}
		s.f = f
		s.path = path
	}
	return s.f.Write(p)
}

// close flushes and closes the spill file when one was created.
func (s *spiller) close() error {
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// createSpill opens a fresh spill file under os.TempDir/ajent-<session>.
func createSpill(sessionID string) (*os.File, string, error) {
	if sessionID == "" {
		sessionID = "anon"
	}
	dir := filepath.Join(os.TempDir(), "ajent-"+sanitize(sessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("bash-%s.txt", randSuffix())
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", err
	}
	return f, filepath.Join(dir, name), nil
}

// sanitize keeps session ids safe as directory names.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// randSuffix returns a short random hex suffix for unique spill names.
func randSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.Itoa(os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// stripANSI removes ANSI escape sequences from s so captured output stays clean.
func stripANSI(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b { // ESC starts an escape sequence to skip over
			out = append(out, s[i])
			continue
		}
		switch s[min(i+2, len(s)-1)] {
		case '[': // CSI ... <final byte>
			i = csiEnd(s, i) - 1
		case ']': // OSC ... BEL or ST terminator
			i = oscEnd(s, i) - 1
		default: // two-byte escape; drop both bytes
			i++
		}
	}
	return string(out)
}

// csiEnd returns the index of the final byte of a CSI sequence starting at esc.
func csiEnd(s string, esc int) int {
	for k := esc + 2; k < len(s); k++ {
		if s[k] >= '@' && s[k] <= '~' {
			return k
		}
	}
	return len(s)
}

// oscEnd returns the index of an OSC sequence's terminator, or len(s).
func oscEnd(s string, esc int) int {
	for k := esc + 2; k < len(s); k++ {
		if s[k] == 0x07 || (s[k] == 0x1b && k+1 < len(s) && s[k+1] == '\\') {
			return k
		}
	}
	return len(s)
}
