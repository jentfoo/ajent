package session

import (
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/jentfoo/ajent/pkg/config"
)

// Writer appends entries to one JSONL file. A single Write per line under a
// mutex keeps the transcript line-atomic for its writer; Sync is the turn-
// boundary fsync, never called by Append.
type Writer struct {
	f      *os.File // nil for Discard
	path   string   // transcript file; empty for Discard, drives HEAD persistence
	head   string   // last appended entry id (or the rewind target after SetHead)
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

// Open reopens an existing file for append and recovers the head from its tail.
func Open(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	e, _, rerr := Read(path)
	if rerr != nil {
		_ = f.Close()
		return nil, rerr
	}
	return &Writer{f: f, path: path, head: headFor(path, e)}, nil
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
	e := Entry{
		ID:       NewID(),
		ParentID: w.head,
		Type:     typ,
		TS:       clock().UnixMilli(),
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
// both histories; nothing is deleted, and the new tip becomes the persisted HEAD.
func (w *Writer) SetHead(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.head = id
	w.persistHeadLocked()
}

// persistHeadLocked writes the HEAD cursor so resume continues from this branch.
// Best-effort: a failed cursor write never fails an append or sync. Caller holds
// the lock; no-op on Discard writers whose path is empty.
func (w *Writer) persistHeadLocked() {
	if w.path == "" || w.head == "" {
		return
	}
	_ = WriteHead(w.path, w.head)
}

// Sync flushes the file at a turn boundary and records the current head.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.f == nil {
		return nil
	}
	w.persistHeadLocked()
	return w.f.Sync()
}

// Close releases the underlying file. Idempotent; safe on Discard writers.
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
