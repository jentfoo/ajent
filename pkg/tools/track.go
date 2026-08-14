package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"sync"
	"time"
)

// ErrNotRead is returned when a write or edit targets a file the session has
// never read.
var ErrNotRead = errors.New("file was not read this session")

// Record is what a tool observed about a file when it last read it.
type Record struct {
	ModTime time.Time
	Size    int64
	Hash    string // sha256 of the content
}

// Tracker records what the session has read so write and edit can detect a file
// that changed underneath them. Safe for concurrent use.
type Tracker struct {
	mu sync.Mutex
	m  map[string]Record
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker { return &Tracker{m: make(map[string]Record)} }

// Observe records the state of data read from path, along with its file info.
func (t *Tracker) Observe(path string, data []byte, info os.FileInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = make(map[string]Record)
	}
	var mod time.Time
	var size int64
	if info != nil {
		mod = info.ModTime()
		size = info.Size()
	}
	sum := sha256.Sum256(data)
	t.m[path] = Record{ModTime: mod, Size: size, Hash: hex.EncodeToString(sum[:])}
}

// Check returns ErrNotRead when path was never read and ErrStale when the file
// changed since it was observed.
func (t *Tracker) Check(path string) error {
	t.mu.Lock()
	rec, ok := t.m[path]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotRead, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("tools: cannot stat %s: %w", path, err)
	}
	data, err := readAllFile(path)
	if err != nil {
		return fmt.Errorf("tools: cannot re-read %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != rec.Hash || fi.ModTime() != rec.ModTime || fi.Size() != rec.Size {
		return ErrStale(path)
	}
	return nil
}

// Records returns a snapshot of the observed paths and their records.
func (t *Tracker) Records() map[string]Record {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		return map[string]Record{}
	}
	return maps.Clone(t.m)
}

// ErrStale reports that a file changed after it was read and must be re-read.
type errStale struct{ path string }

func (e errStale) Error() string {
	return fmt.Sprintf("tools: %s changed since it was read; read it again", e.path)
}
func (e errStale) Unwrap() error { return nil }

// ErrStale returns the stale-file sentinel for path.
func ErrStale(path string) error { return errStale{path: path} }

// IsStale reports whether err is a stale-file error, matching by value or wrap.
func IsStale(err error) bool {
	var e errStale
	return errors.As(err, &e)
}

// readAllFile reads the whole file at path.
func readAllFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}
