package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"slices"
	"sync"
	"time"
)

// Record is what a tool observed about a file when it last read it.
type Record struct {
	ModTime time.Time
	Size    int64
	Hash    string      // sha256 of the content
	edited  []lineRange // lines this session's edits wrote, measured against Hash
}

// Tracker records what the session has observed so @ref expansion can dedupe
// against an unchanged in-context read, and which lines this session's edits have
// written. Safe for concurrent use.
type Tracker struct {
	mu sync.Mutex
	m  map[string]Record
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{m: make(map[string]Record)}
}

// markEdited adds rs to the lines edits have written in path. The ranges hang off
// the current observation, so the next Observe of new content drops them.
func (t *Tracker) markEdited(path string, rs []lineRange) {
	if len(rs) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.m[path]
	if !ok {
		return // no observation to measure the lines against
	}
	rec.edited = append(rec.edited, rs...)
	t.m[path] = rec
}

// editedFor returns the lines edits have written in path, empty unless data is
// still the content those line numbers were measured against.
func (t *Tracker) editedFor(path string, data []byte) []lineRange {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.m[path]
	if !ok || rec.Hash != hashBytes(data) {
		return nil
	}
	return slices.Clone(rec.edited)
}

// hashBytes returns the hex sha256 of data.
func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Observe records the state of data read from path, along with its file info.
// Content identical to the prior observation keeps its recorded edit lines,
// since those numbers still name the same text.
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
	rec := Record{ModTime: mod, Size: size, Hash: hashBytes(data)}
	if prev, ok := t.m[path]; ok && prev.Hash == rec.Hash {
		rec.edited = prev.edited
	}
	t.m[path] = rec
}

// Unchanged reports whether path was observed earlier in the session and still
// matches what was recorded. It is false for a file never observed.
func (t *Tracker) Unchanged(path string) bool {
	t.mu.Lock()
	rec, ok := t.m[path]
	t.mu.Unlock()
	if !ok {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return hashBytes(data) == rec.Hash && fi.ModTime().Equal(rec.ModTime) && fi.Size() == rec.Size
}

// Reset drops every observation. Call it when the context no longer reflects
// what was read, so an @ reference re-injects instead of deduping against it.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.m)
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
