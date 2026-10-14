package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/jentfoo/ajent/pkg/config"
)

// Writer appends entries to one JSONL file with line-atomic writes, fsyncing at turn boundaries.
type Writer struct {
	f      *os.File // nil for Discard
	path   string   // transcript file, empty for Discard, drives HEAD persistence
	head   string   // last appended entry id (or the rewind target after SetHead)
	n      int      // entry count, persisted with the cursor to spot unsynced appends
	closed bool     // further appends error after Close

	mu sync.Mutex
}

// Create makes a new file at path and writes its session entry first.
func Create(path string, d SessionData) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, config.SecretPerm)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f, path: path}
	e, err := w.Append(TypeSession, d)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w.head = e.ID
	return w, nil
}

// Open reopens an existing file for append and recovers the head from its
// persisted cursor or the tail. A torn tail from a crashed partial write is
// quarantined to <path>.torn and truncated before the first append.
func Open(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	if err := repairTornTail(path, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	e, _, rerr := Read(path)
	if rerr != nil {
		_ = f.Close()
		return nil, rerr
	}
	return &Writer{f: f, path: path, head: headFor(path, e), n: len(e)}, nil
}

// tornScanWindow is the tail slice read to gate and locate a torn line.
const tornScanWindow = 64 << 10

// tornSuffix names the sidecar quarantining a partial write's bytes.
const tornSuffix = ".torn"

// tornPath returns the quarantine sidecar beside the transcript at sessionPath.
func tornPath(sessionPath string) string { return sessionPath + tornSuffix }

// repairTornTail fences a partial trailing write: the unterminated bytes are
// copied to a <path>.torn sidecar and the transcript is truncated back to its
// last complete line, so the next append cannot glue onto them. Healthy files
// are left byte-identical; the scan touches only the tail.
func repairTornTail(path string, f *os.File) error {
	rf, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = rf.Close() }()

	fi, err := rf.Stat()
	if err != nil || fi.Size() == 0 {
		return err
	}
	size := fi.Size()
	tail := make([]byte, min(size, int64(tornScanWindow)))
	if _, err := rf.ReadAt(tail, size-int64(len(tail))); err != nil {
		return err
	}
	if tail[len(tail)-1] == '\n' {
		return nil // terminated tail, nothing torn
	}
	cut := size - int64(len(tail)) // bytes to keep, or 0 for a whole-file tear
	if i := bytes.LastIndexByte(tail, '\n'); i >= 0 {
		cut += int64(i) + 1
	} else if size > int64(len(tail)) {
		// torn line longer than the window: rare, worth the full backward scan
		cut, err = lastNewline(rf, size)
		if err != nil {
			return err
		}
	}
	if err := quarantine(rf, tornPath(path), cut, size); err != nil {
		return err
	}
	return f.Truncate(cut)
}

// quarantine copies the torn bytes at offset cut..size into the sidecar.
func quarantine(rf *os.File, dst string, cut, size int64) error {
	sf, err := os.OpenFile(dst, os.O_APPEND|os.O_CREATE|os.O_WRONLY, config.SecretPerm)
	if err != nil {
		return err
	}
	_, err = io.Copy(sf, io.NewSectionReader(rf, cut, size-cut))
	err = errors.Join(err, sf.Close())
	return err
}

// lastNewline scans rf backward chunk by chunk for the final newline. Only
// reached when a torn line exceeds the fast window; 0 means no newline at all.
func lastNewline(rf *os.File, size int64) (int64, error) {
	var buf [tornScanWindow]byte
	for end := size; end > 0; {
		start := max(end-int64(tornScanWindow), 0)
		n, err := rf.ReadAt(buf[:end-start], start)
		if n == 0 && err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i), nil
		}
		end = start
	}
	return 0, nil
}

// Discard returns a writer with no file so callers stay branch-free.
func Discard() *Writer { return &Writer{} }

// Append stamps ParentID from the current head and writes one line. The new id
// becomes the head only after a successful write, or immediately when discarding.
func (w *Writer) Append(typ Type, data any) (Entry, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return Entry{}, err
	}

	// build and write under the lock so concurrent appends form one linear chain
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return Entry{}, errors.New("session writer is closed")
	}
	// TS shares the id's monotonic millisecond so ordering metadata matches append order
	id, ts := newULID()
	e := Entry{
		ID:       id,
		ParentID: w.head,
		Type:     typ,
		TS:       ts,
		Data:     payload,
	}
	line, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	line = append(line, '\n')
	if w.f != nil {
		if _, werr := w.f.Write(line); werr != nil {
			return Entry{}, werr
		}
	}
	w.head = e.ID
	w.n++
	return e, nil
}

// Path returns the transcript file path, or empty for a Discard writer.
func (w *Writer) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.path
}

// Head returns the last appended entry id.
func (w *Writer) Head() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.head
}

// SetHead rewinds to id so later appends branch from it. The transcript keeps
// both histories and nothing is deleted, while the new tip becomes the persisted
// cursor. An empty id starts a new root and drops the cursor instead.
func (w *Writer) SetHead(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.head = id
	// a failure here resurfaces from the next Sync, which rewrites the same cursor
	_ = w.persistHeadLocked()
}

// persistHeadLocked records the branch cursor so resume continues from here.
// Caller holds the lock, and it is a no-op on Discard writers whose path is empty.
func (w *Writer) persistHeadLocked() error {
	if w.path == "" {
		return nil
	} else if w.head == "" {
		return removeHead(w.path) // a new root has no branch to point at
	}
	return writeHead(w.path, w.head, w.n)
}

// Sync fsyncs the file at a turn boundary, then records the current head.
// Data flushes first so the cursor never names an entry that is not durable.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed || w.f == nil {
		return nil
	}
	return errors.Join(w.f.Sync(), w.persistHeadLocked())
}

// Close releases the underlying file. Idempotent and safe on Discard writers.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true
	if w.f == nil {
		return nil
	}
	return w.f.Close()
}
