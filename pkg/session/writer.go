package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/config"
)

// ErrTranscriptGone reports appends against a transcript another process
// deleted: the writer's fd still names the unlinked inode, so without this
// check appends would succeed into a file nothing can read again.
var ErrTranscriptGone = errors.New("transcript deleted by another instance; recording disabled")

// Writer appends entries to one JSONL file with line-atomic writes, fsyncing at turn boundaries.
type Writer struct {
	f       *os.File
	rf      *os.File         // read handle for tail checks, nil when unavailable
	path    string           // transcript file, drives the head slot
	head    string           // last appended entry id (or the rewind target after SetHead)
	headTS  int64            // head entry's stamp, the slot's primary order key
	ts      map[string]int64 // entry id -> stamp, so SetHead slots keep ordering
	n       int              // entry count when opened, for callers reporting progress
	closed  bool             // further appends error after Close
	gone    bool             // transcript deleted under this writer, seen at a path check
	token   string           // this writer's slot token, minted at open
	started int64            // unix millis the slot was minted at

	peers   func(joined, left []int) // peer-visibility observer, nil drops
	peerSet map[string]int           // last scan's live peers, token -> pid

	stop    chan struct{} // closed to retire the slot toucher
	stopped chan struct{} // closed by the toucher on its way out

	mu sync.Mutex
}

// Create makes a new file at path and writes its session entry first.
func Create(path string, d SessionData) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, config.SecretPerm)
	if err != nil {
		return nil, err
	}
	w, err := newWriter(f, path, "", 0, nil)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := w.Append(TypeSession, d); err != nil {
		_ = w.Close()
		return nil, err
	}
	w.syncSlot() // name the session entry at once: an empty live head is nothing to adopt
	return w, nil
}

// Open reopens an existing file for append and recovers the head from its
// writer slots or the tail, treating the open as an explicit target. A torn
// tail from a crashed partial write is quarantined to <path>.torn and truncated
// before the first append, under the cross-writer flock.
func Open(path string) (*Writer, error) {
	w, _, _, err := OpenEntries(path)
	return w, err
}

// OpenEntries is Open also returning the post-repair entries its head recovery
// parsed, with Read's per-line warnings, so a resume rebuilds state without
// reading the file again. The open counts as explicit: a named target never
// adopts a live peer's head, while crash recovery still applies.
func OpenEntries(path string) (*Writer, []Entry, []string, error) {
	return openEntries(path, true)
}

// OpenEntriesImplicit is OpenEntries for an implicit resume (--continue), which
// adopts the newest live slot head, so a second writer forks from the running
// tip and diverges independently: the collaborative behavior, not an error.
func OpenEntriesImplicit(path string) (*Writer, []Entry, []string, error) {
	return openEntries(path, false)
}

func openEntries(path string, explicit bool) (*Writer, []Entry, []string, error) {
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
	head, headTS := resolveOpenHead(path, e, explicit)
	w, werr := newWriter(f, path, head, headTS, e)
	if werr != nil {
		_ = f.Close()
		return nil, nil, nil, werr
	}
	return w, e, warns, nil
}

// newWriter stamps one open transcript with a fresh live slot before any entry
// can append, which is what closes the deletion races, and starts the toucher.
func newWriter(f *os.File, path, head string, headTS int64, entries []Entry) (*Writer, error) {
	w := &Writer{
		f: f, path: path, head: head, headTS: headTS, n: len(entries),
		token: NewID(), started: clock().UnixMilli(),
		peerSet: map[string]int{},
		stop:    make(chan struct{}), stopped: make(chan struct{}),
	}
	if path != "" {
		if rf, rerr := os.Open(path); rerr == nil {
			w.rf = rf // best effort: tail checks degrade to append-only without it
		}
	}
	if len(entries) > 0 {
		w.ts = make(map[string]int64, len(entries))
		for _, e := range entries {
			w.ts[e.ID] = e.TS
		}
	}
	if err := w.writeSlotLocked(); err != nil {
		return nil, err
	}
	go w.touchLoop()
	return w, nil
}

// touchLoop rewrites the slot on the freshness cadence so liveness survives
// quiet stretches. Close retires it.
func (w *Writer) touchLoop() {
	defer close(w.stopped)
	t := time.NewTicker(slotTouch)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			w.mu.Lock()
			if !w.closed {
				_ = w.writeSlotLocked()
			}
			w.mu.Unlock()
		case <-w.stop:
			return
		}
	}
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

// Append stamps ParentID from the current head and writes one line. The new id
// becomes the head only after a successful write. The write takes the
// cross-writer flock, so concurrent processes land interleaved whole lines, and
// re-fences the tail inside the same lock: a peer's torn write landing after
// this writer's open repair is quarantined rather than glued onto. A transcript
// deleted by a --delete under this writer fails with ErrTranscriptGone instead
// of appending into the unlinked inode, so the loss surfaces as a persistence
// notice rather than silent success.
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
	w.head = e.ID
	w.headTS = ts
	if w.ts != nil {
		w.ts[e.ID] = ts
	}
	w.n++
	return e, nil
}

// checkGoneLocked notices a transcript deleted under this writer and latches
// it: once gone, appends fail, the slot stops rewriting (so the heads sidecar
// is not resurrected beside a missing file) and Sync skips its bookkeeping.
// Only a missing path counts, matching Remove; other stat errors read as
// transient. Caller holds the lock.
func (w *Writer) checkGoneLocked() bool {
	if w.path == "" || w.gone {
		return w.gone
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

// Token returns this writer's slot token: the per-instance identity the control
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

// syncSlot stamps the slot now rather than waiting for the toucher, so a
// concurrent resume never sees a head this writer already moved past.
func (w *Writer) syncSlot() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		_ = w.writeSlotLocked()
	}
}

// SetHead rewinds to id so later appends branch from it. The transcript keeps
// both histories and nothing is deleted, while the new tip becomes the slot
// head. An empty id starts a new root; the slot carries the empty head, ordered
// by slot write time.
func (w *Writer) SetHead(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.head = id
	w.headTS = w.entryTS(id)
	// a failure here resurfaces from the next Sync, which rewrites the same slot
	_ = w.writeSlotLocked()
}

// entryTS resolves an entry's stamp, re-reading the transcript when the id is
// absent from the open-time map: a rewind can target a peer writer's entry the
// picker read live from the file, and a zero stamp would order that slot
// oldest. Caller holds the lock.
func (w *Writer) entryTS(id string) int64 {
	if id == "" {
		return 0
	}
	if ts, ok := w.ts[id]; ok {
		return ts
	}
	entries, _, err := Read(w.path)
	if err != nil {
		return 0
	}
	if w.ts == nil {
		w.ts = make(map[string]int64, len(entries))
	}
	for _, e := range entries {
		w.ts[e.ID] = e.TS
	}
	return w.ts[id]
}

// Sync fsyncs the file at a turn boundary, then records the current head.
// Data flushes first so the slot never names an entry that is not durable. The
// peer scan rides the same boundary; its observer runs after the lock is
// released, since it may re-enter the hub and the hub reads the writer head
// under the writer lock.
func (w *Writer) Sync() error {
	w.mu.Lock()
	if w.closed || w.checkGoneLocked() {
		w.mu.Unlock()
		return nil
	}
	err := errors.Join(w.f.Sync(), w.writeSlotLocked())
	var joined, left []int
	if w.peers != nil {
		joined, left = w.scanPeersLocked()
	}
	w.mu.Unlock()
	if len(joined) > 0 || len(left) > 0 {
		w.peers(joined, left)
	}
	return err
}

// writeSlotLocked rewrites this writer's slot, active and naming the current
// head. A gone transcript writes nothing, so its heads sidecar is not
// resurrected. Caller holds the lock.
func (w *Writer) writeSlotLocked() error {
	if w.path == "" || w.gone {
		return nil
	}
	return writeSlot(w.path, headSlot{
		Token: w.token, Head: w.head, EntryTS: w.headTS, Entries: w.n,
		PID: os.Getpid(), Started: w.started, Active: true,
	})
}

// LivePeers counts the other live writers on this transcript, excluding this
// writer's own slot. Exit-time cleanup deletes only when it answers zero.
func (w *Writer) LivePeers() int {
	now := clock()
	mine := w.token
	peers := bulk.SliceFilter(func(sf slotFresh) bool {
		return sf.slot.Token != mine && slotLive(sf.slot, sf.mod, now)
	}, readFreshSlots(w.path))
	return len(peers)
}

// WatchPeers installs the peer-visibility observer: joined and left carry the
// peers' pids, the callback runs on the syncing goroutine and must not block or
// append. Nil clears it.
func (w *Writer) WatchPeers(fn func(joined, left []int)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.peers = fn
	w.peerSet = map[string]int{} // rescan from a clean slate under the new observer
}

// scanPeersLocked diffs the heads directory against the last scan and returns
// the joined and left peers' pids. Caller holds the lock.
func (w *Writer) scanPeersLocked() (joined, left []int) {
	now := clock()
	live := map[string]int{} // token -> pid, live peers as of this scan
	for _, sf := range readFreshSlots(w.path) {
		if sf.slot.Token != w.token && slotLive(sf.slot, sf.mod, now) {
			live[sf.slot.Token] = sf.slot.PID
		}
	}
	for tok, pid := range live {
		if _, ok := w.peerSet[tok]; !ok {
			joined = append(joined, pid)
		}
	}
	for tok, pid := range w.peerSet {
		if _, ok := live[tok]; !ok {
			left = append(left, pid)
		}
	}
	w.peerSet = live
	return joined, left
}

// Close releases the underlying file and marks the slot inactive, so exit never
// blocks --delete for the freshness window. The inactive stamp joins the return
// so a failed write surfaces instead of silently reading live for ten minutes.
// Idempotent.
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
	if w.path != "" && !w.gone {
		// Active false: a clean exit reads as gone immediately
		err = errors.Join(err, writeSlot(w.path, headSlot{
			Token: w.token, Head: w.head, EntryTS: w.headTS, Entries: w.n,
			PID: os.Getpid(), Started: w.started,
		}))
	}
	w.mu.Unlock()

	close(w.stop)
	<-w.stopped
	return err
}
