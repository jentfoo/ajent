package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/config"
)

// maxHistoryLines caps the persisted editor-history file, newest kept.
const maxHistoryLines = 1000

// historyFileName is the per-workspace message record inside a session directory.
const historyFileName = "editor-history.lines"

// histLine is one persisted message plus whether it may be offered for recall.
// Hidden lines (e.g. an argv bootstrap prompt) are durable but never surface in
// Up/Down or Ctrl+R, so they stay distinct from editor-typed messages.
type histLine struct {
	msg    string
	hidden bool // persisted yet excluded from Recent()/RecallIndex
}

// EditorHistory persists every submitted editor message for one workspace in its
// sessions directory. Appends are immediate and atomic under the OS, so they are
// safe from concurrent agents on the same workspace; compaction rewrites with
// last-writer-wins because losing at most a few recall entries is cheaper than a lock.
type EditorHistory struct {
	path         string
	secretPrefix string

	writeFile func(path string, data []byte, perm os.FileMode) error // seam for tests

	mu         sync.Mutex
	added      []histLine // appends not yet durable (write failed), capped at maxHistoryLines
	compacting bool       // a compaction goroutine is in flight; don't start another
}

// NewEditorHistory returns the workspace's editor-history store inside its session dir.
func NewEditorHistory(s *Store, workspace, secretPrefix string) (*EditorHistory, error) {
	dir, err := s.Dir(workspace)
	if err != nil {
		return nil, err
	}
	return &EditorHistory{path: filepath.Join(dir, historyFileName), secretPrefix: secretPrefix,
		writeFile: config.WriteFileAtomic}, nil
}

// Append records a submitted editor message immediately and offers it for recall.
func (h *EditorHistory) Append(msg string) { h.append(msg, false) }

// AppendHidden records a submitted message durably but excludes it from Up/Down and
// Ctrl+R, so non-editor inputs stay distinct from typed lines. A nil receiver is
// a no-op.
func (h *EditorHistory) AppendHidden(msg string) { h.append(msg, true) }

// append writes msg durably and offers it for recall. Blank and secret-prefixed
// messages are skipped; a failed write keeps the line recallable this session.
func (h *EditorHistory) append(msg string, hidden bool) {
	if h == nil || msg == "" { // nil receiver keeps callers free of guards
		return
	}
	msg = strings.TrimRight(msg, "\r")
	if msg == "" || (h.secretPrefix != "" && strings.HasPrefix(msg, h.secretPrefix)) {
		return
	}
	l := histLine{msg: msg, hidden: hidden}

	f, err := os.OpenFile(h.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, config.SecretPerm)
	if err == nil {
		// a single short write is atomic under the OS; two agents never interleave bytes
		if _, werr := f.Write(encodeHistLine(l)); werr != nil {
			err = werr
		}
		_ = f.Close()
	}
	if err != nil { // not yet durable: recall this session and retry on Compact
		h.mu.Lock()
		h.added = append(h.added, l)
		if n := len(h.added) - maxHistoryLines; n > 0 { // a failing disk stays bounded
			h.added = h.added[n:] // the oldest are the least recallable anyway
		}
		h.mu.Unlock()
	}
}

// Recent returns the workspace's submitted messages newest first, deduplicated to
// each text's most recent occurrence and capped at maxHistoryLines. Messages appended
// by this process but not yet durable are included without re-reading. A missing or
// unreadable file yields nil.
func (h *EditorHistory) Recent() []string {
	if h == nil {
		return nil
	}
	lines := readHistLines(h.path)

	h.mu.Lock()
	if len(h.added) > 0 { // unflushed local appends only, never double-counting disk rows
		lines = append(lines, h.added...)
	}
	overgrown := !h.compacting && len(lines) > 2*maxHistoryLines
	if overgrown {
		h.compacting = true // guard before launching so only one compactor runs
	}
	h.mu.Unlock()

	out := normalize(lines, h.secretPrefix) // oldest first, deduped, capped at maxHistoryLines
	// newest-first for recall, dropping hidden rows (e.g. argv bootstrap prompts)
	var recalled []string
	for i := len(out) - 1; i >= 0; i-- {
		if !out[i].hidden {
			recalled = append(recalled, out[i].msg)
		}
	}

	if overgrown {
		go h.Compact() // self-heal after a crash off the caller's path (the UI lock)
	}
	return recalled
}

// Compact rewrites the file to a merged, deduplicated, capped form of whatever is
// on disk plus this process's unflushed appends. It holds no lock against concurrent
// agents: last writer wins and their in-window messages are lost at most once per
// compaction. A write failure keeps the appends queued for the next compaction.
// A nil receiver is a no-op.
func (h *EditorHistory) Compact() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	defer func() { h.compacting = false }() // cleared on every exit path, even a skipped rewrite

	data, err := os.ReadFile(h.path)
	var lines []histLine
	switch {
	case errors.Is(err, os.ErrNotExist):
		// nothing on disk yet; compaction is just the local appends
	case err != nil:
		return // cannot read current state; leave the file alone
	default:
		for _, row := range strings.Split(string(data), "\n") {
			if l, ok := decodeHistLine(row); ok {
				lines = append(lines, l)
			}
		}
	}
	// queued appends ride along; they leave the queue only once durably written
	lines = append(lines, h.added...)

	out := normalize(lines, h.secretPrefix)
	if len(out) == 0 { // nothing to persist; don't create a phantom empty file
		return
	}
	var buf bytes.Buffer
	for _, m := range out { // one JSON row per message so multi-line turns round-trip whole
		buf.Write(encodeHistLine(m))
	}
	if err := h.writeFile(h.path, buf.Bytes(), config.SecretPerm); err != nil {
		return // not durable: the queue stays for the next compaction's retry
	}
	h.added = nil // durable: the queue is flushed
}

// encodeHistLine marshals one message to a single physical row so multi-line turns
// round-trip whole. Hidden rows persist as an object so their exclusion survives a
// restart and compaction; visible rows stay bare JSON strings (hand-edit friendly).
func encodeHistLine(l histLine) []byte {
	var b []byte
	if l.hidden {
		b, _ = json.Marshal(struct {
			Msg    string `json:"msg"`
			Hidden bool   `json:"hidden"`
		}{l.msg, true})
	} else {
		b, _ = json.Marshal(l.msg)
	}
	return append(b, '\n')
}

// readHistLines decodes every row of path back to its message and hidden flag.
// A missing or unreadable file yields nil; blank rows are skipped.
func readHistLines(path string) []histLine {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []histLine
	for _, row := range strings.Split(string(data), "\n") {
		if l, ok := decodeHistLine(row); ok {
			out = append(out, l)
		}
	}
	return out
}

// decodeHistLine parses one physical row back to its message and hidden flag. A
// non-JSON row (hand-edited or a plain-text leftover) is kept literally so the file
// stays human-edit friendly.
func decodeHistLine(row string) (histLine, bool) {
	row = strings.TrimRight(row, "\r")
	if row == "" {
		return histLine{}, false // blank rows carry no message
	}
	var msg string
	if json.Unmarshal([]byte(row), &msg) == nil { // visible rows are bare JSON strings
		return histLine{msg: msg}, true
	}
	var h struct {
		Msg    string `json:"msg"`
		Hidden bool   `json:"hidden"`
	}
	if json.Unmarshal([]byte(row), &h) == nil { // hidden rows are objects
		return histLine{msg: h.Msg, hidden: h.Hidden}, true
	}
	return histLine{msg: row}, true // plain-text leftover
}

// normalize trims CRs, drops blank/secret lines, keeps each text's newest occurrence,
// then caps at maxHistoryLines from the newest. A text stays visible when any copy was
// typed; purely hidden texts stay excluded.
func normalize(lines []histLine, secretPrefix string) []histLine {
	var clean []histLine
	for _, l := range lines {
		m := strings.TrimRight(l.msg, "\r")
		if m != "" && (secretPrefix == "" || !strings.HasPrefix(m, secretPrefix)) {
			clean = append(clean, histLine{msg: m, hidden: l.hidden})
		}
	}

	// keep each text's newest occurrence; a text is recallable when any copy was typed.
	newestPos := make(map[string]int, len(clean))
	visibleSet := make(map[string]struct{}, len(clean)) // texts with at least one visible copy
	for i, l := range clean {
		newestPos[l.msg] = i
		if !l.hidden {
			visibleSet[l.msg] = struct{}{}
		}
	}

	var out []histLine
	for i, l := range clean {
		if newestPos[l.msg] != i { // skip copies older than the newest occurrence
			continue
		}
		_, anyVisible := visibleSet[l.msg]
		out = append(out, histLine{msg: l.msg, hidden: !anyVisible})
	}
	if len(out) > maxHistoryLines {
		out = out[len(out)-maxHistoryLines:]
	}
	return out
}
