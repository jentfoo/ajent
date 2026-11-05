package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"

	"github.com/jentfoo/ajent/pkg/config"
)

// ErrTranscriptGone reports appends against a transcript another process
// deleted: the writer's fd still names the unlinked inode, so without this
// check appends would succeed into a file nothing can read again.
var ErrTranscriptGone = errors.New("transcript deleted by another instance; recording disabled")

// Writer appends entries to one JSONL file with line-atomic writes, fsyncing at turn boundaries.
type Writer struct {
	f      *os.File
	rf     *os.File    // read handle for tail checks, nil when unavailable
	path   string      // transcript file; every writer owns one, never empty
	head   string      // last appended entry id, or the rewind target after SetHead
	closed bool        // further appends error after Close
	gone   bool        // transcript deleted under this writer, seen at a path check
	watch  func(Entry) // optional observer of successful appends, app supplied
	token  string      // per-open identity the control socket names itself by

	mu sync.Mutex
}

// Create makes a new file at path and writes its session entry first.
func Create(path string, d SessionData) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, config.SecretPerm)
	if err != nil {
		return nil, err
	}
	w := newWriter(f, path, "")
	if _, err := w.Append(TypeSession, d); err != nil {
		_ = w.Close()
		return nil, err
	}
	return w, nil
}

// Open reopens an existing file for append and recovers the head from the
// file (see resolveHead). A torn tail from a crashed partial write is
// quarantined to <path>.torn and truncated before the first append, under the
// cross-writer flock.
func Open(path string) (*Writer, error) {
	w, _, _, err := OpenEntries(path)
	return w, err
}

// OpenEntries is Open also returning the post-repair entries its head recovery
// parsed, with Read's per-line warnings, so a resume rebuilds state without
// reading the file again.
func OpenEntries(path string) (*Writer, []Entry, []string, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := repairTornTail(path, f); err != nil {
		_ = f.Close()
		return nil, nil, nil, err
	}
	e, warns, rerr := Read(path)
	if rerr != nil {
		_ = f.Close()
		return nil, nil, nil, rerr
	}
	return newWriter(f, path, resolveHead(e)), e, warns, nil
}

// newWriter wraps one opened transcript for append.
func newWriter(f *os.File, path, head string) *Writer {
	w := &Writer{f: f, path: path, head: head, token: NewID()}
	if rf, rerr := os.Open(path); rerr == nil {
		w.rf = rf // best effort: tail checks degrade to append-only without it
	}
	return w
}

// tornScanWindow is the tail slice read to gate and locate a torn line.
const tornScanWindow = 64 << 10

// tornSuffix names the sidecar quarantining a partial write's bytes.
const tornSuffix = ".torn"

// legacyHeadSuffix names the pre-cursor head sidecar an older build wrote.
const legacyHeadSuffix = ".head"

// tornPath returns the quarantine sidecar beside the transcript at sessionPath.
func tornPath(sessionPath string) string { return sessionPath + tornSuffix }

// repairTornTail fences a partial trailing write: the unterminated bytes are
// copied to a <path>.torn sidecar and the transcript is truncated back to its
// last complete line, so the next append cannot glue onto them. Healthy files
// are left byte-identical; the scan touches only the tail. The exclusive flock
// spans stat, read, quarantine and truncate, so a concurrent appender's line
// can never land inside the window being cut.
func repairTornTail(path string, f *os.File) error {
	if err := lockWriter(f); err != nil {
		return err
	}
	defer unlockWriter(f)
	return repairTornTailLocked(path, f)
}

// repairTornTailLocked is repairTornTail for a caller already holding the
// cross-writer flock. Caller holds the flock.
func repairTornTailLocked(path string, f *os.File) error {
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
	var cut int64 // bytes to keep, 0 for a whole-file tear
	if i := bytes.LastIndexByte(tail, '\n'); i >= 0 {
		cut = size - int64(len(tail)) + int64(i) + 1
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

// lastNewline returns the offset one past the final newline in the first size
// bytes of rf, scanning backward chunk by chunk, or 0 when the file has none.
// Only reached when a torn line exceeds the fast window.
func lastNewline(rf *os.File, size int64) (int64, error) {
	var buf [tornScanWindow]byte
	for end := size; end > 0; {
		start := max(end-int64(tornScanWindow), 0)
		n, err := rf.ReadAt(buf[:end-start], start)
		if n == 0 && err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}

// WatchAppend installs fn to observe each entry this writer successfully
// appends; nil clears it. The observer fires on the appending goroutine after
// the write completes and must not block or call back into the writer.
func (w *Writer) WatchAppend(fn func(Entry)) {
	w.mu.Lock()
	w.watch = fn
	w.mu.Unlock()
}

// Append stamps ParentID from the current head and writes one line, then
// announces it to any observer. The new id becomes the head only after a
// successful write. The write takes the cross-writer flock, so concurrent
// processes land interleaved whole lines, and re-fences the tail inside the
// same lock: a peer's torn write landing after this writer's open repair is
// quarantined rather than glued onto. A transcript deleted by a --delete under
// this writer fails with ErrTranscriptGone instead of appending into the
// unlinked inode, so the loss surfaces as a persistence notice rather than
// silent success.
func (w *Writer) Append(typ Type, data any) (Entry, error) {
	e, err := w.append(typ, data)
	if err != nil {
		return e, err
	}
	w.mu.Lock()
	fn := w.watch
	w.mu.Unlock()
	if fn != nil {
		fn(e)
	}
	return e, nil
}

// append performs the write itself, all of it under w.mu.
func (w *Writer) append(typ Type, data any) (Entry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.appendLocked(typ, data)
}

// appendLocked writes one entry. Caller holds w.mu.
func (w *Writer) appendLocked(typ Type, data any) (Entry, error) {
	var payload []byte
	if data != nil {
		var err error
		if payload, err = json.Marshal(data); err != nil {
			return Entry{}, err
		}
	}

	// build and write under the lock so concurrent appends form one linear chain
	if w.closed {
		return Entry{}, errors.New("session writer is closed")
	}
	if w.checkGoneLocked() {
		return Entry{}, ErrTranscriptGone
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
	if lerr := lockWriter(w.f); lerr != nil {
		return Entry{}, lerr
	}
	if !w.tailTerminated() {
		if rerr := repairTornTailLocked(w.path, w.f); rerr != nil {
			unlockWriter(w.f)
			return Entry{}, rerr
		}
	}
	if _, werr := w.f.Write(line); werr != nil {
		unlockWriter(w.f)
		return Entry{}, werr
	}
	unlockWriter(w.f)
	if typ != TypeHead { // a cursor is a chain marker, never the live head
		w.head = e.ID
	}
	return e, nil
}

// checkGoneLocked notices a transcript deleted under this writer and latches
// it: once gone, appends fail, so nothing writes beside a missing file. Only a
// missing path counts, matching Remove; other stat errors read as transient.
// Caller holds the lock.
func (w *Writer) checkGoneLocked() bool {
	if w.gone {
		return true
	}
	if _, err := os.Stat(w.path); errors.Is(err, fs.ErrNotExist) {
		w.gone = true
	}
	return w.gone
}

// Path returns the transcript file path.
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

// Token returns this writer's per-open token, the identity the control
// socket and registry derive their names from.
func (w *Writer) Token() string {
	return w.token
}

// tailTerminated reports whether the file's last byte ends a line. Best
// effort: an unreadable or empty tail reads as terminated so appends never
// fail on the check itself. Caller holds the flock.
func (w *Writer) tailTerminated() bool {
	if w.rf == nil {
		return true
	}
	fi, err := w.rf.Stat()
	if err != nil || fi.Size() == 0 {
		return true
	}
	var one [1]byte
	if _, err := w.rf.ReadAt(one[:], fi.Size()-1); err != nil {
		return true
	}
	return one[0] == '\n'
}

// SetHead rewinds to id so later appends fork from it, recording one cursor
// entry (see TypeHead). An empty id starts a new root. The transcript keeps
// both histories and nothing is deleted. A cursor write that fails degrades to
// losing the move across a restart, never the live branch.
func (w *Writer) SetHead(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.head = id
	_, _ = w.appendLocked(TypeHead, nil) // not Append: a cursor is bookkeeping, never announced
}

// Sync fsyncs the file at a turn boundary.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.checkGoneLocked() {
		return nil
	}
	return w.f.Sync()
}

// Close releases the underlying file. Idempotent.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	err := w.f.Close()
	if w.rf != nil {
		_ = w.rf.Close()
		w.rf = nil
	}
	w.mu.Unlock()
	return err
}
