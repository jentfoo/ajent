package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"sync"
	"time"
)

// Record is what a tool observed about a file when it last read it.
type Record struct {
	ModTime time.Time
	Size    int64
	Hash    string // sha256 of the content
}

// Tracker records what the session has observed so @ref expansion can dedupe
// against an unchanged in-context read. Safe for concurrent use.
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
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == rec.Hash && fi.ModTime().Equal(rec.ModTime) && fi.Size() == rec.Size
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
