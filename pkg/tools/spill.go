package tools

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
)

// maxSpillAttempts bounds how many fresh suffixes are tried before giving up on
// an EEXIST collision.
const maxSpillAttempts = 8

// spiller lazily creates a per-session spill file in os.TempDir for tool output
// that exceeds the in-memory budget, so a normal command leaves nothing behind.
type spiller struct {
	sessionID string
	prefix    string // names the file kind: "bash", "grep"
	f         *os.File
	path      string

	failed bool  // a create or write failed: no footer may point at this spill
	err    error // the first failure, reported to every later writer
}

// spillSeq backs the rand-failure fallback so same-PID spills stay unique per call.
var spillSeq atomic.Uint64

// newSpiller returns a spiller writing into a session-named temp directory. It
// is not created until Write first needs it.
func newSpiller(sessionID, prefix string) *spiller {
	if prefix == "" {
		prefix = "bash"
	}
	return &spiller{sessionID: sessionID, prefix: prefix}
}

// Write appends p to the spill file, creating both lazily on first use.
func (s *spiller) Write(p []byte) (int, error) {
	if s.failed {
		return 0, s.err // dead: later writes report the recorded failure
	}
	if s.f == nil {
		f, path, err := createSpill(s.sessionID, s.prefix)
		if err != nil {
			s.markFailed(err)
			return 0, err
		}
		s.f = f
		s.path = path
	}
	n, err := s.f.Write(p)
	if err != nil {
		s.markFailed(err) // a short or failed write leaves partial data behind: drop it
	}
	return n, err
}

// markFailed kills the spiller once and removes any partial file, so nothing on
// disk claims to hold output no footer may point at.
func (s *spiller) markFailed(err error) {
	if s.failed {
		return
	}
	s.failed = true
	s.err = err
	if s.path != "" {
		_ = os.Remove(s.path)
	}
}

// claim reports what a truncation footer may honestly say about this spill:
// complete only when the file was created and every byte written reached it.
func (s *spiller) claim() spillClaim {
	if s.failed {
		return spillClaim{state: spillFailed}
	}
	if s.path != "" {
		return spillClaim{path: s.path, state: spillComplete}
	}
	var none spillClaim // nothing spilled or attempted
	return none
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

// discardSpill closes and removes an eagerly created spill file that will never
// hold output, the eager-file counterpart of markFailed. f and path come from a
// successful createSpill.
func discardSpill(f *os.File, path string) {
	_ = f.Close()
	if path != "" {
		_ = os.Remove(path)
	}
}

// createSpill opens a fresh spill file under os.TempDir/ajent-<session>.
func createSpill(sessionID, prefix string) (*os.File, string, error) {
	if sessionID == "" {
		sessionID = "anon"
	}
	dir := filepath.Join(os.TempDir(), "ajent-"+sanitize(sessionID))
	return openSpill(dir, prefix, randSuffix)
}

// openSpill creates the spill dir and opens a fresh exclusive file, retrying with
// a new suffix when the name is taken so concurrent spills never share a path.
func openSpill(dir, prefix string, suffix func() string) (*os.File, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	namePrefix := sanitize(prefix)
	var lastErr error
	for range maxSpillAttempts {
		name := fmt.Sprintf("%s-%s.txt", namePrefix, suffix())
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, fs.ErrExist) {
			lastErr = err
			continue
		}
		if err != nil { // a real failure: no file and no path may leak upward
			return nil, "", err
		}
		return f, filepath.Join(dir, name), nil
	}
	return nil, "", lastErr
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
		return fallbackSuffix()
	}
	return hex.EncodeToString(b[:])
}

// fallbackSuffix keeps same-PID spills unique when crypto/rand is unavailable.
func fallbackSuffix() string {
	return strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(spillSeq.Add(1), 10)
}
