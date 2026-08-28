package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

	mu         sync.Mutex
	added      []histLine // appended this process, oldest first
	compacting bool       // a compaction goroutine is in flight; don't start another
}

// NewEditorHistory returns the workspace's editor-history store inside its session dir.
func NewEditorHistory(s *Store, workspace, secretPrefix string) (*EditorHistory, error) {
	dir, err := s.Dir(workspace)
	if err != nil {
		return nil, err
	}
	return &EditorHistory{path: filepath.Join(dir, historyFileName), secretPrefix: secretPrefix}, nil
}

// Append records a submitted editor message immediately and offers it for recall.
func (h *EditorHistory) Append(msg string) { h.append(msg, false) }

// AppendHidden records a submitted message durably but excludes it from Up/Down and
// Ctrl+R, so non-editor inputs stay distinct from typed lines. A nil receiver is
// a no-op.
func (h *EditorHistory) AppendHidden(msg string) { h.append(msg, true) }

// append writes msg to disk immediately and remembers it for this process's recall.
// Messages prefixed by the secret marker and blank messages never reach disk; errors
// are dropped because history is best effort. A nil receiver is a no-op.
func (h *EditorHistory) append(msg string, hidden bool) {
	if h == nil || msg == "" { // nil receiver keeps main.go free of guards
		return
	}
	msg = strings.TrimRight(msg, "\r")
	if msg == "" || (h.secretPrefix != "" && strings.HasPrefix(msg, h.secretPrefix)) {
		return
	}
	l := histLine{msg: msg, hidden: hidden}
	h.mu.Lock()
	h.added = append(h.added, l)
	h.mu.Unlock()

	f, err := os.OpenFile(h.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, config.SecretPerm)
	if err != nil {
		return
	}
	// a single short write is atomic under the OS; two agents never interleave bytes
	defer func() { _ = f.Close() }()
	_, _ = f.Write(encodeHistLine(l))
}

// Recent returns the workspace's submitted messages newest first, deduplicated to
// each text's most recent occurrence and capped at maxHistoryLines. Messages appended
// by this process but not yet flushed are included without re-reading. A missing or
// unreadable file yields nil.
func (h *EditorHistory) Recent() []string {
	if h == nil {
		return nil
	}
	lines := readHistLines(h.path)

	h.mu.Lock()
	if len(h.added) > 0 {
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
// on disk plus this process's appends. It holds no lock against concurrent agents:
// last writer wins and their in-window messages are lost at most once per compaction.
// A nil receiver is a no-op.
func (h *EditorHistory) Compact() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	data, err := os.ReadFile(h.path)
	var lines []histLine
	switch {
	case errors.Is(err, os.ErrNotExist):
		lines = slices.Clone(h.added) // nothing on disk yet; compact just the local appends
	case err != nil:
		h.compacting = false
		return // cannot read current state; leave the file alone
	default:
		for _, row := range strings.Split(string(data), "\n") {
			if l, ok := decodeHistLine(row); ok {
				lines = append(lines, l)
			}
		}
	}
	if len(h.added) > 0 {
		lines = append(lines, h.added...)
		h.added = nil // flushed into the rewrite
	}

	out := normalize(lines, h.secretPrefix)
	if len(out) == 0 { // nothing to persist; don't create a phantom empty file
		return
	}
	var buf bytes.Buffer
	for _, m := range out { // one JSON row per message so multi-line turns round-trip whole
		buf.Write(encodeHistLine(m))
	}
	_ = config.WriteFileAtomic(h.path, buf.Bytes(), config.SecretPerm)
	h.compacting = false
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

// normalize trims CRs, drops blank and secret-prefixed lines, keeps only the most
// recent occurrence of each line (preserving its hidden flag), then caps at
// maxHistoryLines from the newest.
func normalize(lines []histLine, secretPrefix string) []histLine {
	var clean []histLine
	for _, l := range lines {
		m := strings.TrimRight(l.msg, "\r")
		if m != "" && (secretPrefix == "" || !strings.HasPrefix(m, secretPrefix)) {
			clean = append(clean, histLine{msg: m, hidden: l.hidden})
		}
	}
	// dedup to the most recent copy: walk backwards keeping first-seen lines,
	// then reverse to restore original order.
	var out []histLine
	seen := make(map[string]struct{}, len(clean))
	for i := len(clean) - 1; i >= 0; i-- {
		l := clean[i]
		if _, ok := seen[l.msg]; ok {
			continue
		}
		seen[l.msg] = struct{}{}
		out = append(out, l)
	}
	slices.Reverse(out)
	if len(out) > maxHistoryLines {
		out = out[len(out)-maxHistoryLines:]
	}
	return out
}
